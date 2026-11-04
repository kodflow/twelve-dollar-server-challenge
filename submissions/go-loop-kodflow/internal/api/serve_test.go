package api

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kitsunium/sdk/pkg/v1/net/http1"
	"github.com/kitsunium/sdk/pkg/v1/net/server"
)

// fakeDB answers every job with its kind, echoing a create's text and user,
// and records what it saw. It is called from the loop's thread; the mutex is
// for the test goroutine reading it.
type fakeDB struct {
	mu      sync.Mutex
	seen    []Kind
	batches int
}

func (f *fakeDB) ExecBatch(b *Batch) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batches++
	b.Out = b.Out[:0]
	// Grown once, so the bodies taken below stay where they are.
	b.Out = append(b.Out, make([]byte, 0, 256*len(b.Jobs))...)
	for i := range b.Jobs {
		j := &b.Jobs[i]
		f.seen = append(f.seen, j.Kind)
		off := len(b.Out)
		if j.Kind == KindCreatePost {
			text := b.In[j.TextOff : j.TextOff+j.TextLen]
			user := b.In[j.UserOff : j.UserOff+j.UserLen]
			b.Out = fmt.Appendf(b.Out, `{"kind":%d,"text":%q,"user":%q}`, j.Kind, text, user)
			j.Status = 201
		} else {
			b.Out = fmt.Appendf(b.Out, `{"kind":%d}`, j.Kind)
			j.Status = 200
		}
		j.Body = b.Out[off:len(b.Out):len(b.Out)]
	}
}

func (f *fakeDB) snapshot() ([]Kind, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Kind(nil), f.seen...), f.batches
}

// startLoop serves h on a loop group bound to a free local port and returns
// its address.
func startLoop(t *testing.T, h *Handler) string {
	t.Helper()
	p, err := http1.NewParser(http1.Limits{Head: MaxHeaderBytes, Body: MaxBodyBytes})
	if err != nil {
		t.Fatal(err)
	}
	srv := server.New(server.WithDrainTimeout(time.Second))
	srv.Group("http",
		server.Listen("tcp", "127.0.0.1:0"),
		server.Plaintext(),
		server.Loop(server.LoopValue{Loops: 1, SpillCap: MaxHeaderBytes + MaxBodyBytes}),
	).HandleLoop(http1.HandleLoop(h, p))
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(t.Context()) })
	return srv.State().Listeners[0].Address
}

func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestServeLoopPipelined sends three requests in one write, one of them split
// across a second write, and expects three ordered responses: a deferred
// one, a static one between, and a deferred one after.
func TestServeLoopPipelined(t *testing.T) {
	db := &fakeDB{}
	h := NewHandler(newAuth(t), 1)
	h.DB = db
	c := dial(t, startLoop(t, h))

	if _, err := io.WriteString(c, "GET /feed HTTP/1.1\r\nHost: x\r\n\r\nGET /nope?a=1 HTTP/1.1\r\nHost: x\r\n\r\nGET /posts/5 HT"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := io.WriteString(c, "TP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	want := []struct {
		status int
		body   string
	}{{200, `{"kind":2}`}, {404, `{"error":"not found"}`}, {200, `{"kind":3}`}}
	for i, w := range want {
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Fatalf("response %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != w.status || string(body) != w.body || resp.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("response %d: %d %s %v", i, resp.StatusCode, body, resp.Header)
		}
		if i == len(want)-1 && !resp.Close {
			t.Fatal("last response should close the connection")
		}
	}
	if _, err := r.ReadByte(); err != io.EOF {
		t.Fatalf("connection still open after close: %v", err)
	}
	seen, _ := db.snapshot()
	if len(seen) != 2 || seen[0] != KindFeed || seen[1] != KindGetPost {
		t.Fatalf("executor saw %v", seen)
	}
}

// TestServeLoopBatchesPipelined checks that pipelined database requests that
// arrive together share one executor call, and that a create's text and
// username reach the executor intact although the next request reuses the
// loop's claims buffer.
func TestServeLoopBatchesPipelined(t *testing.T) {
	db := &fakeDB{}
	h := NewHandler(newAuth(t), 1)
	h.DB = db
	c := dial(t, startLoop(t, h))

	exp := time.Now().Add(time.Hour).Unix()
	tokA := hs256(t, fmt.Sprintf(`{"sub":"3","username":"alice","exp":%d}`, exp))
	tokB := hs256(t, fmt.Sprintf(`{"sub":"4","username":"bob","exp":%d}`, exp))
	post := func(tok, text string) string {
		body := fmt.Sprintf(`{"body":%q}`, text)
		return fmt.Sprintf("POST /posts HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: %d\r\n\r\n%s", tok, len(body), body)
	}
	all := post(tokA, "  first  ") + post(tokB, "second") + "GET /health HTTP/1.1\r\nHost: x\r\n\r\n"
	if _, err := io.WriteString(c, all); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	want := []string{
		`{"kind":4,"text":"first","user":"alice"}`,
		`{"kind":4,"text":"second","user":"bob"}`,
		`{"kind":1}`,
	}
	for i, w := range want {
		resp, err := http.ReadResponse(r, nil)
		if err != nil {
			t.Fatalf("response %d: %v", i, err)
		}
		body, _ := io.ReadAll(resp.Body)
		if string(body) != w {
			t.Fatalf("response %d: %d %s", i, resp.StatusCode, body)
		}
	}
	seen, batches := db.snapshot()
	if len(seen) != 3 || batches > 2 {
		t.Fatalf("executor saw %v in %d batches", seen, batches)
	}
}

// TestServeLoopLargeRequest sends a body larger than one read and expects it
// framed whole: the token is checked first, so 401.
func TestServeLoopLargeRequest(t *testing.T) {
	h := NewHandler(newAuth(t), 1)
	h.DB = &fakeDB{}
	c := dial(t, startLoop(t, h))
	body := `{"x":"` + strings.Repeat("a", 40000) + `"}`
	go func() {
		_, _ = fmt.Fprintf(c, "POST /posts HTTP/1.1\r\nHost: x\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	}()
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

// TestServeLoopRefusal checks that a request the profile refuses is answered
// by the codec and closes the connection without reaching the handler.
func TestServeLoopRefusal(t *testing.T) {
	db := &fakeDB{}
	h := NewHandler(newAuth(t), 1)
	h.DB = db
	c := dial(t, startLoop(t, h))
	if _, err := io.WriteString(c, "POST /posts HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\nGET /feed HTTP/1.1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	resp, err := http.ReadResponse(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 400 || !resp.Close {
		t.Fatalf("status %d close %v", resp.StatusCode, resp.Close)
	}
	_, _ = io.ReadAll(resp.Body)
	if _, err := r.ReadByte(); err != io.EOF {
		t.Fatalf("connection still open after refusal: %v", err)
	}
	if seen, _ := db.snapshot(); len(seen) != 0 {
		t.Fatalf("executor saw %v", seen)
	}
}
