// Command userload runs bench/load.js's user model from Go, for the steps
// one k6 generator cannot drive: past one source address's ports, and past
// what k6's per-VU memory and CPU allow on one box.
//
//	userload -url TARGET_HOST:80 -users 140000 -ramp 120 -hold 300 \
//	  -tokens tokens.json -local 192.0.2.9,192.0.2.10,192.0.2.11 -out summary.json
//
// (TARGET_HOST and the 192.0.2.0/24 sources are placeholders, RFC 5737.)
//
// A user is load.js's loop on one keep-alive connection: GET /feed, think
// 3-7 s, GET /posts/:id of one of its posts, think 3-8 s, like it (15 %),
// create a post (2 %), idle 5-15 s; a first iteration waits 0-5 s first. A
// connection the server closes, or answers with Connection: close, is dialled
// again before the next request, as k6's transport does. Users ramp linearly
// over -ramp, hold for -hold, then stop over -rampdown, last started first
// (k6's ramp-down; 0 stops every user at once). A request fails on a
// network error, a 60 s timeout (k6's default) or an unexpected status, as
// load.js's checks and http_req_failed count them.
//
// The summary has the k6 thresholds' inputs over the whole run and over the
// hold alone: requests, failures, p95 and p99 of the durations (ms). -idle
// replaces the user loop with one GET /health per connection, then silence:
// what an idle keep-alive connection costs the server. -timeline writes, per
// second, the requests that ended in it (count, failures, p50, p99, max) and
// the slow ones (over 1 s) by the second they started in. -linger0 closes a
// stopping user's connection with a reset, so the generator keeps no
// TIME_WAIT that the next run's connect(2) would have to step over.
//
// Give each process few enough connections per -local address: past about
// half of ip_local_port_range in use (open or in TIME_WAIT), connect(2) on
// the generator spent 90 % and more of its CPU in the kernel picking a
// source port, and the run measured the generator (BENCH.md, S15).
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const reqTimeout = 60 * time.Second

type token struct {
	Token    string `json:"token"`
	Username string `json:"username"`
}

// stats collects one phase's request outcomes, every duration kept at
// 0.1 ms resolution for the percentiles.
type stats struct {
	mu    sync.Mutex
	reqs  int64
	fails int64
	byErr map[string]int64
	durs  []int32 // ms × 10
}

func (s *stats) add(d time.Duration, fail string) {
	s.mu.Lock()
	s.reqs++
	if fail != "" {
		s.fails++
		s.byErr[fail]++
	}
	s.durs = append(s.durs, int32(d/(100*time.Microsecond)))
	s.mu.Unlock()
}

func (s *stats) summary() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	sort.Slice(s.durs, func(i, j int) bool { return s.durs[i] < s.durs[j] })
	pct := func(p float64) float64 {
		if len(s.durs) == 0 {
			return 0
		}
		i := int(p * float64(len(s.durs)-1))
		return float64(s.durs[i]) / 10
	}
	rate := 0.0
	if s.reqs > 0 {
		rate = float64(s.fails) / float64(s.reqs)
	}
	return map[string]any{
		"reqs": s.reqs, "fails": s.fails, "fail_rate": rate, "errors": s.byErr,
		"p50_ms": pct(0.50), "p95_ms": pct(0.95), "p99_ms": pct(0.99), "max_ms": pct(1),
	}
}

type run struct {
	linger0  bool
	addr     string
	locals   []*net.TCPAddr
	tokens   []token
	all      stats
	hold     stats
	inHold   atomic.Bool
	stopping atomic.Bool
	open     atomic.Int64
	dials    atomic.Int64
	dialErrs atomic.Int64
	nextIP   atomic.Uint64
}

type conn struct {
	r  *run
	c  net.Conn
	br *bufio.Reader
	wb []byte
}

func (r *run) dial() (net.Conn, error) {
	d := net.Dialer{Timeout: reqTimeout, Control: noPortBind}
	if len(r.locals) > 0 {
		d.LocalAddr = r.locals[r.nextIP.Add(1)%uint64(len(r.locals))]
	}
	r.dials.Add(1)
	c, err := d.Dial("tcp", r.addr)
	if err != nil {
		r.dialErrs.Add(1)
		return nil, err
	}
	return c, nil
}

func (c *conn) close() {
	if c.c != nil && c.r.linger0 && c.r.stopping.Load() {
		if tc, ok := c.c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
	}
	if c.c != nil {
		c.c.Close()
		c.c = nil
		c.r.open.Add(-1)
	}
}

// do sends one request on the user's connection, dialling first when it has
// none, and returns the status and body. One retry on a reused connection
// the server had already closed, as Go's transport does for a request it
// never wrote.
func (c *conn) do(method, path, auth string, body []byte) (int, []byte, error) {
	for attempt := 0; ; attempt++ {
		reused := c.c != nil
		if c.c == nil {
			nc, err := c.r.dial()
			if err != nil {
				return 0, nil, fmt.Errorf("dial: %w", err)
			}
			c.c = nc
			c.r.open.Add(1)
			c.br = bufio.NewReaderSize(nc, 4096)
		}
		c.wb = c.wb[:0]
		c.wb = append(c.wb, method...)
		c.wb = append(c.wb, ' ')
		c.wb = append(c.wb, path...)
		c.wb = append(c.wb, " HTTP/1.1\r\nHost: bench\r\nUser-Agent: userload\r\n"...)
		if auth != "" {
			c.wb = append(c.wb, "Authorization: Bearer "...)
			c.wb = append(c.wb, auth...)
			c.wb = append(c.wb, "\r\nContent-Type: application/json\r\n"...)
		}
		if body != nil || method == "POST" {
			c.wb = append(c.wb, "Content-Length: "...)
			c.wb = strconv.AppendInt(c.wb, int64(len(body)), 10)
			c.wb = append(c.wb, "\r\n"...)
		}
		c.wb = append(c.wb, "\r\n"...)
		c.wb = append(c.wb, body...)
		c.c.SetDeadline(time.Now().Add(reqTimeout))
		if _, err := c.c.Write(c.wb); err != nil {
			c.close()
			if reused && attempt == 0 {
				continue
			}
			return 0, nil, fmt.Errorf("write: %w", err)
		}
		status, resp, closeAfter, err := readResponse(c.br)
		if err != nil {
			c.close()
			// Nothing read on a reused connection: the server closed it
			// while it idled; Go's transport retries such a request.
			if reused && attempt == 0 && errors.Is(err, errNothingRead) {
				continue
			}
			return 0, nil, err
		}
		if closeAfter {
			c.close()
		}
		return status, resp, nil
	}
}

var errNothingRead = errors.New("connection closed before any byte")

func readResponse(br *bufio.Reader) (status int, body []byte, closeAfter bool, err error) {
	line, err := br.ReadSlice('\n')
	if err != nil {
		if len(line) == 0 && (err == io.EOF || isReset(err)) {
			return 0, nil, false, errNothingRead
		}
		return 0, nil, false, fmt.Errorf("read status: %w", err)
	}
	if len(line) < 12 || !bytes.HasPrefix(line, []byte("HTTP/1.1 ")) {
		return 0, nil, false, fmt.Errorf("bad status line %q", line)
	}
	status, err = strconv.Atoi(string(line[9:12]))
	if err != nil {
		return 0, nil, false, fmt.Errorf("bad status %q", line)
	}
	clen := -1
	for {
		h, err := br.ReadSlice('\n')
		if err != nil {
			return 0, nil, false, fmt.Errorf("read header: %w", err)
		}
		if len(h) <= 2 {
			break
		}
		k, v, ok := bytes.Cut(h, []byte(":"))
		if !ok {
			continue
		}
		v = bytes.TrimSpace(v)
		switch strings.ToLower(string(k)) {
		case "content-length":
			clen, _ = strconv.Atoi(string(v))
		case "connection":
			closeAfter = bytes.EqualFold(v, []byte("close"))
		}
	}
	if clen < 0 {
		return 0, nil, false, errors.New("no content-length")
	}
	body = make([]byte, clen)
	if _, err := io.ReadFull(br, body); err != nil {
		return 0, nil, false, fmt.Errorf("read body: %w", err)
	}
	return status, body, closeAfter, nil
}

func isReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET) || errors.Is(err, net.ErrClosed)
}

// timeline buckets every request by the second it ended in, and counts the
// slow ones (over 1 s) by the second they started in (S15 diagnosis).
type timeline struct {
	mu   sync.Mutex
	end  map[int64][]int32
	fail map[int64]int
	slow map[int64]int
}

var tl = timeline{end: map[int64][]int32{}, fail: map[int64]int{}, slow: map[int64]int{}}

func (t *timeline) add(start time.Time, d time.Duration, fail string) {
	e := start.Add(d).Unix()
	t.mu.Lock()
	t.end[e] = append(t.end[e], int32(d/(100*time.Microsecond)))
	if fail != "" {
		t.fail[e]++
	}
	if d > time.Second {
		t.slow[start.Unix()]++
	}
	t.mu.Unlock()
}

func (t *timeline) write(path string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	t.mu.Lock()
	defer t.mu.Unlock()
	secs := []int64{}
	for k := range t.end {
		secs = append(secs, k)
	}
	for k := range t.slow {
		if _, ok := t.end[k]; !ok {
			secs = append(secs, k)
		}
	}
	sort.Slice(secs, func(i, j int) bool { return secs[i] < secs[j] })
	fmt.Fprintln(f, "sec n fails p50_ms p99_ms max_ms slow_started")
	for _, k := range secs {
		v := t.end[k]
		sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		q := func(p float64) float64 {
			if len(v) == 0 {
				return 0
			}
			return float64(v[int(p*float64(len(v)-1))]) / 10
		}
		fmt.Fprintf(f, "%d %d %d %.1f %.1f %.1f %d\n", k, len(v), t.fail[k], q(0.5), q(0.99), q(1), t.slow[k])
	}
	return nil
}

func (r *run) record(start time.Time, fail string) {
	d := time.Since(start)
	tl.add(start, d, fail)
	r.all.add(d, fail)
	if r.inHold.Load() {
		r.hold.add(d, fail)
	}
}

func errClass(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "timeout"):
		return "timeout"
	case strings.Contains(s, "reset"):
		return "reset"
	case strings.Contains(s, "refused"):
		return "refused"
	case strings.Contains(s, "EOF"):
		return "eof"
	case strings.HasPrefix(s, "dial"):
		return "dial"
	}
	return "other"
}

func between(lo, hi float64) time.Duration {
	return time.Duration((lo + rand.Float64()*(hi-lo)) * float64(time.Second))
}

// sleep waits d unless the run stops first; it reports whether to go on.
func (r *run) sleep(d time.Duration, stop <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-stop:
		return false
	}
}

// user is load.js's default function, looped until stop.
func (r *run) user(vu int, stop <-chan struct{}) {
	tok := r.tokens[(vu-1)%len(r.tokens)]
	c := &conn{r: r}
	defer c.close()
	if !r.sleep(between(0, 5), stop) {
		return
	}
	for iter := 0; ; iter++ {
		start := time.Now()
		st, body, err := c.do("GET", "/feed", "", nil)
		fail := ""
		var feed struct {
			Posts []struct {
				ID int64 `json:"id"`
			} `json:"posts"`
		}
		switch {
		case err != nil:
			fail = errClass(err)
		case st != 200:
			fail = "status_" + strconv.Itoa(st)
		default:
			if json.Unmarshal(body, &feed) != nil || len(feed.Posts) == 0 {
				fail = "feed_json"
			}
		}
		r.record(start, fail)
		if fail != "" {
			if !r.sleep(between(3, 7), stop) {
				return
			}
			continue
		}
		post := feed.Posts[rand.IntN(len(feed.Posts))].ID
		if !r.sleep(between(3, 7), stop) {
			return
		}
		start = time.Now()
		st, _, err = c.do("GET", "/posts/"+strconv.FormatInt(post, 10), "", nil)
		r.record(start, outcome(st, err, 200))
		if !r.sleep(between(3, 8), stop) {
			return
		}
		if rand.Float64() < 0.15 {
			start = time.Now()
			st, _, err = c.do("POST", "/posts/"+strconv.FormatInt(post, 10)+"/like", tok.Token, nil)
			r.record(start, outcome(st, err, 200, 201))
		}
		if rand.Float64() < 0.02 {
			b, _ := json.Marshal(map[string]string{"body": fmt.Sprintf("%s says hi at %s (VU %d, iter %d)",
				tok.Username, time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), vu, iter)})
			start = time.Now()
			st, _, err = c.do("POST", "/posts", tok.Token, b)
			r.record(start, outcome(st, err, 201))
		}
		if !r.sleep(between(5, 15), stop) {
			return
		}
	}
}

func outcome(st int, err error, ok ...int) string {
	if err != nil {
		return errClass(err)
	}
	for _, s := range ok {
		if st == s {
			return ""
		}
	}
	return "status_" + strconv.Itoa(st)
}

// idler opens its connection, sends one GET /health and holds it, silent.
func (r *run) idler(stop <-chan struct{}) {
	c := &conn{r: r}
	defer c.close()
	start := time.Now()
	st, _, err := c.do("GET", "/health", "", nil)
	r.record(start, outcome(st, err, 200))
	<-stop
}

func main() {
	addr := flag.String("url", "", "target host:port")
	users := flag.Int("users", 1000, "virtual users")
	ramp := flag.Duration("ramp", 60*time.Second, "linear ramp-up")
	hold := flag.Duration("hold", 300*time.Second, "hold")
	tokens := flag.String("tokens", "tokens.json", "seed/tokens.json")
	locals := flag.String("local", "", "comma-separated source addresses, round robin")
	idle := flag.Bool("idle", false, "one GET /health per connection, then silence")
	out := flag.String("out", "", "summary JSON file (stdout when empty)")
	tlPath := flag.String("timeline", "", "per-second latency file")
	rampDown := flag.Duration("rampdown", 0, "linear ramp-down after the hold, last started first")
	linger0 := flag.Bool("linger0", false, "reset stopping users' connections: no TIME_WAIT on the generator")
	raw := flag.String("raw", "", "prefix of the raw duration files (.all, .hold: int32 LE, 0.1 ms), for merging several processes")
	flag.Parse()
	if *addr == "" {
		fmt.Fprintln(os.Stderr, "userload: -url is required")
		os.Exit(2)
	}
	r := &run{addr: *addr, linger0: *linger0}
	r.all.byErr = map[string]int64{}
	r.hold.byErr = map[string]int64{}
	for _, ip := range strings.Split(*locals, ",") {
		if ip = strings.TrimSpace(ip); ip != "" {
			r.locals = append(r.locals, &net.TCPAddr{IP: net.ParseIP(ip)})
		}
	}
	if !*idle {
		b, err := os.ReadFile(*tokens)
		if err != nil {
			fmt.Fprintln(os.Stderr, "userload:", err)
			os.Exit(1)
		}
		if err := json.Unmarshal(b, &r.tokens); err != nil || len(r.tokens) == 0 {
			fmt.Fprintln(os.Stderr, "userload: tokens:", err)
			os.Exit(1)
		}
	}
	// stops[vu] ends user vu: all at once at the hold's end, or one by one
	// over -rampdown, the last started first.
	stops := make([]chan struct{}, *users+1)
	for i := range stops {
		stops[i] = make(chan struct{})
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	var wg sync.WaitGroup
	t0 := time.Now()
	fmt.Fprintf(os.Stderr, "t0 %.3f\n", float64(t0.UnixNano())/1e9)
	progress := time.NewTicker(10 * time.Second)
	defer progress.Stop()
	go func() {
		for range progress.C {
			fmt.Fprintf(os.Stderr, "%5.0fs open %d dials %d dial_errs %d reqs %d fails %d\n",
				time.Since(t0).Seconds(), r.open.Load(), r.dials.Load(), r.dialErrs.Load(),
				atomic.LoadInt64(&r.all.reqs), atomic.LoadInt64(&r.all.fails))
		}
	}()
	// Ramp: user i starts at i/users of the ramp.
	interrupted := false
	for i := 1; i <= *users && !interrupted; i++ {
		at := t0.Add(time.Duration(float64(*ramp) * float64(i-1) / float64(*users)))
		if d := time.Until(at); d > 0 {
			select {
			case <-time.After(d):
			case <-sig:
				interrupted = true
			}
		}
		wg.Add(1)
		go func(vu int) {
			defer wg.Done()
			if *idle {
				r.idler(stops[vu])
			} else {
				r.user(vu, stops[vu])
			}
		}(i)
	}
	holdStart := t0.Add(*ramp)
	if d := time.Until(holdStart); d > 0 && !interrupted {
		select {
		case <-time.After(d):
		case <-sig:
			interrupted = true
		}
	}
	r.inHold.Store(true)
	fmt.Fprintf(os.Stderr, "hold start %.3f open %d\n", float64(time.Now().UnixNano())/1e9, r.open.Load())
	if !interrupted {
		select {
		case <-time.After(*hold):
		case <-sig:
		}
	}
	r.inHold.Store(false)
	openAtEnd := r.open.Load()
	fmt.Fprintf(os.Stderr, "hold end %.3f open %d\n", float64(time.Now().UnixNano())/1e9, openAtEnd)
	r.stopping.Store(true)
	stopAt := time.Now()
	for i := *users; i >= 1; i-- {
		at := stopAt.Add(time.Duration(float64(*rampDown) * float64(*users-i) / float64(*users)))
		if d := time.Until(at); d > 0 {
			time.Sleep(d)
		}
		close(stops[i])
	}
	close(stops[0])
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(70 * time.Second):
	}
	all, hs := r.all.summary(), r.hold.summary()
	if *raw != "" {
		writeRaw(*raw+".all", &r.all)
		writeRaw(*raw+".hold", &r.hold)
	}
	res := map[string]any{
		"users": *users, "ramp_s": ramp.Seconds(), "hold_s": hold.Seconds(), "idle": *idle,
		"local_addrs": len(r.locals), "open_at_hold_end": openAtEnd,
		"dials": r.dials.Load(), "dial_errors": r.dialErrs.Load(),
		"all": all, "hold": hs,
		"pass": all["p95_ms"].(float64) < 500 && all["p99_ms"].(float64) < 1000 && all["fail_rate"].(float64) < 0.01,
	}
	if *tlPath != "" {
		if err := tl.write(*tlPath); err != nil {
			fmt.Fprintln(os.Stderr, "timeline:", err)
		}
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	if *out != "" {
		_ = os.WriteFile(*out, append(b, '\n'), 0o644)
	}
	os.Stdout.Write(append(b, '\n'))
}

// writeRaw writes s's durations, sorted by summary, then its request and
// failure counts as two int64 at the end.
func writeRaw(path string, s *stats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := make([]byte, 0, 4*len(s.durs)+16)
	for _, d := range s.durs {
		b = binary.LittleEndian.AppendUint32(b, uint32(d))
	}
	b = binary.LittleEndian.AppendUint64(b, uint64(s.reqs))
	b = binary.LittleEndian.AppendUint64(b, uint64(s.fails))
	_ = os.WriteFile(path, b, 0o644)
}

// ipBindAddressNoPort is IP_BIND_ADDRESS_NO_PORT (linux/in.h).
const ipBindAddressNoPort = 24

// noPortBind defers the source port to connect(2), which picks it by the
// whole 4-tuple. Without it Go binds the -local address to port 0 first, and
// the kernel's search for a free port grows with the ports in use: past
// ~30,000 connections from one address the generator spent its CPU there.
func noPortBind(network, address string, c syscall.RawConn) error {
	var serr error
	err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, ipBindAddressNoPort, 1)
	})
	if err != nil {
		return err
	}
	return serr
}
