// Command server is the go-loop-kodflow submission to the $12 Server
// Challenge: the kitsunium SDK's loop engine serves the connections, its
// http1 profile frames the requests and its verifier checks the JWTs, and
// each loop batch's database requests run through SQLite in one
// group-committed transaction.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kitsunium/sdk/pkg/v1/app/lifecycle"
	"github.com/kitsunium/sdk/pkg/v1/net/http1"
	"github.com/kitsunium/sdk/pkg/v1/net/server"
	"github.com/kitsunium/sdk/pkg/v1/proc/memlimit"
	"github.com/kitsunium/sdk/pkg/v1/proc/signal"

	"github.com/kodflow/twelve-dollar-go-loop/internal/api"
)

const (
	// maxConns is three io_uring descriptor tables: the SDK derives
	// ⌈maxConns / 65535⌉ rings, and a connection it accepts as a direct
	// descriptor takes no process descriptor, so LimitNOFILE=65535 does not
	// bound it. With SDK v0.26.43, 120,000 users held a 5-minute hold and
	// 140,000 held one hold in six (bench/BENCH.md, S12). Beyond maxConns the
	// SDK accepts and closes.
	maxConns = 196000
	// defaultBatchWait bounds the SDK's grouped wait at 4 ms where its own
	// default is 250 µs: 36.4 against 58.5 µs/req at 10,000 users on one
	// ring, for a p99 of 6.6 ms, and 120,000 users held (bench/BENCH.md, S13).
	defaultBatchWait = 4 * time.Millisecond
	// defaultCQBatch bounds the completions a ring reaps in one round at 64
	// where the SDK's default is 512: a loop turn then reaps at most 768 a
	// ring instead of 6,144, so a backlog no longer comes back as one batch
	// of 1,000-2,500 requests whose answers all wait for its end. At
	// 180,000 users: turns over 40 ms 43 -> 27 a hold, delayed ACKs -15 %,
	// at the same cost per request (bench/BENCH.md, S24). Only from Linux
	// 6.13 on: see sweepCQBatch.
	defaultCQBatch = 64
	// sweepCQBatch is the SDK's own default, kept before Linux 6.13. There a
	// direct descriptor's close puts its file only once older requests
	// completed, so the SDK sweeps: after a quarter of a ring's table in
	// closes it cancels and re-arms every open connection's recv. When users
	// leave in mass, 64 completions a round leave those bursts queued behind
	// the requests still served: on 6.8 at 150,000 users, 21.8 % of the
	// ramp-down's requests took over 1 s at 64 and 7.3 % at 512
	// (bench/BENCH.md, S28, S29).
	sweepCQBatch = 512
	// idleTimeout keeps an idle keep-alive connection for longer than the
	// 65 s the SPEC asks of servers behind Nginx.
	idleTimeout = 120 * time.Second
	// spillCap lets one whole request, head and body at their bounds, wait in
	// a connection's spill while its bytes arrive.
	spillCap = api.MaxHeaderBytes + api.MaxBodyBytes
	// fallbackMemLimit is the soft heap limit when no cgroup cap is found:
	// the box has 2 GB and an OOM loses the run.
	fallbackMemLimit = 1536 << 20
)

// engineBuild is the digest of the C engine library this binary links, set
// by build.sh, for the startup line.
var engineBuild = "unknown"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, `{"event":"fatal","error":%q}`+"\n", err.Error())
		os.Exit(1)
	}
}

func run() error {
	dbPath, secret := os.Getenv("SQLITE_PATH"), os.Getenv("JWT_SECRET")
	if dbPath == "" || secret == "" {
		return fmt.Errorf("SQLITE_PATH and JWT_SECRET must be set")
	}
	addr := net.JoinHostPort(envOr("HOST", "0.0.0.0"), envOr("PORT", "3000"))

	// LOOP_TRACE writes the loop's and the checkpointer's timings per 100 ms
	// to a file, for the bench's diagnosis; unset, nothing is measured.
	if err := startTrace(os.Getenv("LOOP_TRACE")); err != nil {
		return err
	}

	limit := memlimit.Apply()
	if !limit.Applied() && os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(fallbackMemLimit)
	}

	auth, err := api.NewAuth([]byte(secret))
	if err != nil {
		return err
	}

	backend, err := loopBackend(os.Getenv("LOOP_BACKEND"))
	if err != nil {
		return err
	}
	parser, err := http1.NewParser(http1.Limits{Head: api.MaxHeaderBytes, Body: api.MaxBodyBytes})
	if err != nil {
		return err
	}
	// One P per CPU and one more: the SDK never sets GOMAXPROCS, and its
	// LoopsAuto runs one loop per P but one. The P left over is what lets a
	// loop parked in io_uring_enter, or in a cgo call into SQLite, keep its
	// own P, and what turns on the SDK's 5 ms yield (ADR 0199); with one P
	// per loop sysmon retakes and hands off the loop's P on every wait
	// (bench/BENCH.md, S9). An explicit GOMAXPROCS wins.
	if os.Getenv("GOMAXPROCS") == "" {
		runtime.GOMAXPROCS(max(runtime.NumCPU(), 1) + 1)
	}
	loops, err := loopCount(os.Getenv("LOOPS"))
	if err != nil {
		return err
	}
	batchWait, err := loopBatchWait(os.Getenv("LOOP_BATCH_WAIT"))
	if err != nil {
		return err
	}
	conns, err := connCap(os.Getenv("MAX_CONNS"))
	if err != nil {
		return err
	}
	// An unknown token is refused at Start by the SDK.
	caps := loopCaps(os.Getenv("LOOP_CAPS"))

	cqBatch, err := positiveInt("LOOP_CQ_BATCH")
	if err != nil {
		return err
	}
	cqSource := "env"
	if cqBatch == 0 {
		cqBatch, cqSource = kernelCQBatch(kernelRelease())
	}
	budget, err := loopBatchBudget(os.Getenv("LOOP_BATCH_BUDGET"))
	if err != nil {
		return err
	}

	loop := server.LoopValue{Backend: backend, Loops: loops, SpillCap: spillCap, Capabilities: caps, CQBatch: cqBatch}
	setDuration(&loop.BatchWait, batchWait)
	setDuration(&loop.BatchBudget, budget)

	var db *engine
	srv := server.New(server.WithDrainTimeout(3 * time.Second))
	// Sized for every P: LoopsAuto resolves to fewer lanes than that.
	handler := api.NewHandler(auth, max(loops, runtime.GOMAXPROCS(0)))
	srv.Group("http",
		server.Listen(listenNetwork(addr), addr),
		// The box serves plain HTTP; the SDK wants a group without TLS to
		// say so.
		server.Plaintext(),
		server.MaxConns(conns),
		server.IdleTimeout(idleTimeout),
		server.Loop(loop),
	).HandleLoop(http1.HandleLoop(handler, parser))

	stateSig, err := signal.Parse("SIGUSR1")
	if err != nil {
		return err
	}
	var stopState func()

	lc := lifecycle.New(lifecycle.Config{StopTimeout: 10 * time.Second})
	components := []lifecycle.Component{
		{
			Name: "sqlite",
			Start: func(context.Context) error {
				db, err = openEngine(dbPath)
				if err != nil {
					return err
				}
				handler.DB = db
				return nil
			},
			Stop: func(context.Context) error {
				db.close()
				return nil
			},
		},
		{
			Name:  "http",
			Start: srv.Start,
			// No drain: the SDK closes each connection with a shutdown and a
			// linked cancel that io_uring runs on its workers, which took 2 s
			// at 10,000 open connections, 249 s at 60,000 and more than 15 min
			// at 140,000, the process unkillable and the port held meanwhile
			// (bench/BENCH.md, S11). Every answer already sent was committed;
			// the kernel closes the connections when the process ends, 0.3 s
			// at 60,000.
			Stop: func(context.Context) error {
				db.exit(0)
				return nil
			},
		},
		{
			// Last to start, first to stop: the State line printed here on
			// SIGTERM still sees the server as it was serving.
			Name: "state",
			Start: func(context.Context) error {
				printStart(addr, limit, srv, db, loop, cqSource, conns)
				trace.watch(func() (time.Duration, uint64) {
					var p99 time.Duration
					var cut uint64
					for _, l := range srv.State().Loops {
						p99 = max(p99, time.Duration(l.BatchP99))
						for _, r := range l.RingStates {
							cut += r.BatchesCut
						}
					}
					return p99, cut
				})
				var ch <-chan signal.Signal
				ch, stopState = signal.Notify(stateSig)
				go func() {
					for range ch {
						printState("SIGUSR1", srv, db)
					}
				}()
				return nil
			},
			Stop: func(context.Context) error {
				stopState()
				printState("SIGTERM", srv, db)
				return nil
			},
		},
	}
	for _, c := range components {
		if err := lc.Add(c); err != nil {
			return err
		}
	}
	term, err := signal.Parse("SIGTERM")
	if err != nil {
		return err
	}
	interrupt, err := signal.Parse("SIGINT")
	if err != nil {
		return err
	}
	return lifecycle.Run(context.Background(), lc, lifecycle.RunConfig{
		Signals: []signal.Signal{term, interrupt},
		Notify:  true,
	})
}

// loopBackend reads LOOP_BACKEND: empty or "auto" lets the SDK take the best
// backend its probe reaches (io_uring, then epoll on Linux), and the State
// line says which; the others force one, for the bench harness's variants.
func loopBackend(v string) (server.LoopBackend, error) {
	switch v {
	case "", "auto":
		return server.LoopBackendAuto, nil
	case "io_uring":
		return server.LoopBackendIOURing, nil
	case "epoll":
		return server.LoopBackendEpoll, nil
	case "portable":
		return server.LoopBackendPortable, nil
	}
	return 0, fmt.Errorf("LOOP_BACKEND %q: want auto, io_uring, epoll or portable", v)
}

// loopCount reads LOOPS: empty is the SDK's LoopsAuto, one loop per P but
// one (one on the 1-vCPU box), a count for the bench harness's variants
// otherwise.
func loopCount(v string) (int, error) {
	if v == "" {
		return server.LoopsAuto, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("LOOPS %q: want a positive integer", v)
	}
	return n, nil
}

// positiveInt reads the environment variable name: empty is zero, the SDK's
// default, a positive count for the bench harness's variants otherwise.
func positiveInt(name string) (int, error) {
	v := os.Getenv(name)
	if v == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s %q: want a positive integer", name, v)
	}
	return n, nil
}

// kernelRelease is the running kernel's release, empty where
// /proc/sys/kernel/osrelease cannot be read (off Linux).
func kernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// kernelCQBatch is CQBatch when LOOP_CQ_BATCH is unset, and its source for
// the startup line: defaultCQBatch from Linux 6.13 on, sweepCQBatch before.
// The SDK settles whether its rings sweep only at Start, after CQBatch is
// set, and does not report it; the release stands in for it, and one that
// does not parse keeps the SDK's default.
func kernelCQBatch(release string) (int, string) {
	major, rest, ok := strings.Cut(release, ".")
	if !ok {
		return sweepCQBatch, "kernel unknown"
	}
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	maj, err1 := strconv.Atoi(major)
	minor, err2 := strconv.Atoi(rest[:end])
	if err1 != nil || err2 != nil {
		return sweepCQBatch, "kernel unknown"
	}
	if maj > 6 || maj == 6 && minor >= 13 {
		return defaultCQBatch, "kernel >= 6.13"
	}
	return sweepCQBatch, "kernel < 6.13"
}

// loopBatchBudget reads LOOP_BATCH_BUDGET, the SDK's bound on one io_uring
// batch's duration (ADR 0206): empty is its default (none), otherwise a Go
// duration, for the bench harness's variants.
func loopBatchBudget(v string) (time.Duration, error) {
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("LOOP_BATCH_BUDGET %q: want a positive duration", v)
	}
	return d, nil
}

// connCap reads MAX_CONNS: empty is maxConns, a count for the bench
// harness's variants otherwise.
func connCap(v string) (int, error) {
	if v == "" {
		return maxConns, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("MAX_CONNS %q: want a positive integer", v)
	}
	return n, nil
}

// loopBatchWait reads LOOP_BATCH_WAIT, the SDK's grouped io_uring wait
// (ADR 0199, 0202): empty is defaultBatchWait, "off" turns it off,
// otherwise a Go duration, for the bench harness's variants.
func loopBatchWait(v string) (time.Duration, error) {
	switch v {
	case "":
		return defaultBatchWait, nil
	case "off":
		return -1, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("LOOP_BATCH_WAIT %q: want off or a positive duration", v)
	}
	return d, nil
}

// setDuration sets one of the SDK's duration fields, whose type the server
// facade does not name.
// listenNetwork is "tcp4" for an IPv4 HOST, "tcp6" for an IPv6 one, else
// "tcp". Go listens on "tcp" with the IPv4 wildcard through a dual-stack
// IPv6 socket, and every connection it accepts is then a TCPv6 socket, about
// 200 bytes of kernel memory more than a TCP one (bench/BENCH.md, S11).
func listenNetwork(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "tcp"
	}
	ip, err := netip.ParseAddr(host)
	switch {
	case err != nil:
		return "tcp"
	case ip.Is4():
		return "tcp4"
	default:
		return "tcp6"
	}
}

func setDuration[T ~int64](field *T, d time.Duration) {
	*field = T(d)
}

// defaultCaps are the SDK's opt-in io_uring capabilities this server runs
// with: DEFER_TASKRUN with SINGLE_ISSUER (completions are posted only when
// the loop asks for them, no task-work interrupts), the SQ without its
// index array, and the ring descriptor registered with its thread: 63.4 ->
// 60.3 µs/req on the bench box (S9 A/B). Ubuntu 24.04's 6.8 kernel has all
// three; one a kernel lacks is reported as a fault in the State line, and
// nothing refuses to start. Receive bundles (uring-recv-bundle) are left
// off: on this load they cost the same CPU per request and 35-45 MB more
// memory at 180,000 users (bench/BENCH.md, S21).
var defaultCaps = []server.Capability{
	server.CapabilityURingDeferTaskrun,
	server.CapabilityURingNoSQArray,
	server.CapabilityURingRingFDs,
}

// loopCaps reads LOOP_CAPS: empty is defaultCaps, "none" opts into
// nothing, otherwise a comma-separated list of the SDK's tokens, for the
// bench harness's variants.
func loopCaps(v string) []server.Capability {
	switch v {
	case "":
		return defaultCaps
	case "none":
		return nil
	}
	var caps []server.Capability
	for _, tok := range strings.Split(v, ",") {
		caps = append(caps, server.Capability(strings.TrimSpace(tok)))
	}
	return caps
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// printStart writes the one startup line to stderr. The loop group's backend,
// as the probe settled it, is in the server's loops state.
func printStart(addr string, limit memlimit.Limit, srv *server.Server, db *engine, loop server.LoopValue, cqSource string, conns int) {
	var nofile syscall.Rlimit
	_ = syscall.Getrlimit(syscall.RLIMIT_NOFILE, &nofile)
	backends := []string{}
	for _, l := range srv.State().Loops {
		backends = append(backends, l.Backend.String())
	}
	line := map[string]any{
		"event":      "start",
		"backend":    backends,
		"model":      "sdk loop engine + http1, one transaction per loop batch",
		"loops":      srv.State().Loops,
		"listen":     addr,
		"network":    listenNetwork(addr),
		"go":         runtime.Version(),
		"gomaxprocs": runtime.GOMAXPROCS(0),
		"numcpu":     runtime.NumCPU(),
		"memlimit": map[string]any{
			"source":    fmt.Sprint(limit.Source),
			"allowance": limit.Allowance,
			"limit":     debug.SetMemoryLimit(-1),
		},
		"rlimit_nofile":     map[string]uint64{"soft": uint64(nofile.Cur), "hard": uint64(nofile.Max)},
		"max_conns":         conns,
		"max_batch":         maxBatch,
		"checkpoint_frames": checkpointFrames,
		"db":                db.info,
		"engine_build":      engineBuild,
		"loop_config":       loopConfig(srv, loop, cqSource),
	}
	writeLine(line)
}

// loopConfig is the loop engine's pacing as the SDK resolves it, which its
// State does not report: the grouped wait, io_uring only (off when negative,
// 250 µs for 32 completions when zero, never past the 10 ms tick), and the
// 5 ms yield, on while GOMAXPROCS exceeds the group's loops (ADR 0199).
func loopConfig(srv *server.Server, loop server.LoopValue, cqSource string) map[string]any {
	cfg := map[string]any{}
	for _, l := range srv.State().Loops {
		wait, waitMin := "off", 0
		if d := time.Duration(loop.BatchWait); d >= 0 && l.Backend == server.LoopBackendIOURing {
			if d == 0 {
				d = 250 * time.Microsecond
			}
			wait, waitMin = min(d, 10*time.Millisecond).String(), 32
		}
		yield := "off"
		if l.GOMAXPROCS > l.Loops {
			yield = (5 * time.Millisecond).String()
		}
		cfg["loops"], cfg["loops_source"] = l.Loops, l.LoopsSource
		cfg["batch_wait"], cfg["batch_wait_min"], cfg["yield"] = wait, waitMin, yield
		cq, budget := loop.CQBatch, "none"
		if cq == 0 {
			cq = 512
		}
		if d := time.Duration(loop.BatchBudget); d > 0 && l.Backend == server.LoopBackendIOURing {
			budget = d.String()
		}
		cfg["cq_batch"], cfg["cq_batch_source"], cfg["batch_budget"] = cq, cqSource, budget
	}
	return cfg
}

// printState writes one State line: the SDK server's State plus the engine
// counters and the Go runtime's memory view.
func printState(trigger string, srv *server.Server, db *engine) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	writeLine(map[string]any{
		"event":  "state",
		"signal": trigger,
		"time":   time.Now().UTC().Format(time.RFC3339Nano),
		"server": srv.State(),
		"engine": db.snapshot(),
		"runtime": map[string]any{
			"goroutines": runtime.NumGoroutine(),
			"heap_inuse": ms.HeapInuse,
			"sys":        ms.Sys,
			"num_gc":     ms.NumGC,
			"gc_cpu":     ms.GCCPUFraction,
		},
	})
}

func writeLine(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(fmt.Sprintf(`{"event":"error","error":%q}`, err.Error()))
	}
	_, _ = os.Stderr.Write(append(b, '\n'))
}
