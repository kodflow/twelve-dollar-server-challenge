// Package api is the HTTP side of the server: routing, request validation and
// JWT checks on requests the SDK's http1 codec framed, and the per-loop batch
// of database work the loop adapter completes at the end of each batch. It
// holds no database code, so it builds and tests without cgo.
package api

import (
	"bytes"

	"github.com/kitsunium/sdk/pkg/v1/net/http1"
)

const (
	// MaxHeaderBytes bounds the request line plus headers.
	MaxHeaderBytes = 16 << 10
	// MaxBodyBytes bounds a request body. The SPEC leaves bodies over 16 KB
	// out of scope; anything larger is refused before it is buffered.
	MaxBodyBytes = 64 << 10
)

// Job is one database request of a loop's batch. The executor fills Status
// and Body.
type Job struct {
	Kind   Kind
	PostID int64
	UserID int64
	// TextOff, TextLen, UserOff and UserLen place a create's trimmed text and
	// the token's username in the batch's In.
	TextOff, TextLen int32
	UserOff, UserLen int32
	Status           int
	Body             []byte
}

// Batch is the database work one loop gathered during one batch of requests.
type Batch struct {
	Jobs []Job
	// In holds the text the jobs point into.
	In []byte
	// Out is the executor's buffer: every Body aliases it, and it is reused by
	// the loop's next batch.
	Out []byte
}

// Executor runs a batch. It returns only once the batch's transaction has
// committed (or every job was answered with an error), so whatever it sets
// may be sent.
type Executor interface {
	ExecBatch(b *Batch)
}

// loopState is what one loop owns. Only that loop's thread touches it, so it
// takes no lock.
type loopState struct {
	scratch *Scratch
	batch   Batch
	// res holds the deferred responses, one per job, in the same order.
	res []*http1.Response
}

// Handler serves the SPEC's routes on the SDK's loop engine: it answers what
// needs no database at once and defers the rest to EndBatch, which runs one
// transaction for the loop's whole batch.
type Handler struct {
	auth  *Auth
	DB    Executor
	loops []loopState
}

// NewHandler returns a handler for a loop group of loops loops.
func NewHandler(auth *Auth, loops int) *Handler {
	h := &Handler{auth: auth, loops: make([]loopState, max(loops, 1))}
	for i := range h.loops {
		h.loops[i].scratch = auth.NewScratch()
	}
	return h
}

// contentType is the one extra field line of every answer.
var contentType = []byte("Content-Type: application/json\r\n")

// ServeHTTP1 implements http1.Handler.
func (h *Handler) ServeHTTP1(req *http1.Request, res *http1.Response) {
	l := &h.loops[req.Lane]
	r := Request{
		Method:  req.Method,
		Path:    req.Target,
		Auth:    req.Authorization,
		HasAuth: req.HasAuthorization,
		Body:    req.Body,
	}
	if q := bytes.IndexByte(r.Path, '?'); q >= 0 {
		r.Path = r.Path[:q]
	}
	op := h.auth.Route(&r, l.scratch)
	res.Header = contentType
	if op.Kind == KindStatic {
		res.Status, res.Body = op.Status, op.Body
		return
	}
	b := &l.batch
	j := Job{Kind: op.Kind, PostID: op.PostID, UserID: op.UserID}
	if op.Kind == KindCreatePost {
		// The username is a view of the loop's claims buffer and the text was
		// decoded from the connection's bytes: both are copied now, before
		// the next request reuses either.
		j.TextOff, j.TextLen = int32(len(b.In)), int32(len(op.Text))
		b.In = append(b.In, op.Text...)
		j.UserOff, j.UserLen = int32(len(b.In)), int32(len(op.User))
		b.In = append(b.In, op.User...)
	}
	b.Jobs = append(b.Jobs, j)
	l.res = append(l.res, res)
	res.Defer = true
}

// EndBatch implements http1.Batcher: one executor call, so one transaction,
// for every database request the loop's batch deferred. The adapter writes
// the answers once it returns, after the commit.
func (h *Handler) EndBatch(loop int) {
	l := &h.loops[loop]
	b := &l.batch
	if len(b.Jobs) == 0 {
		return
	}
	h.DB.ExecBatch(b)
	for i, res := range l.res {
		j := &b.Jobs[i]
		res.Status, res.Body, res.Defer = j.Status, j.Body, false
	}
	clear(l.res)
	l.res = l.res[:0]
	b.Jobs, b.In = b.Jobs[:0], b.In[:0]
}
