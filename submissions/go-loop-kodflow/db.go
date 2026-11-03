package main

/*
#cgo CFLAGS: -I${SRCDIR}/csrc
#cgo LDFLAGS: -L${SRCDIR}/.deps -lengine -lm
#include <stdlib.h>
#include "engine.h"
*/
import "C"

import (
	"errors"
	"os"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/kodflow/twelve-dollar-go-loop/internal/api"
)

const (
	// maxBatch bounds one cgo call and one transaction. A loop batch with
	// more database requests than this runs as several transactions, each
	// committed before any answer of the batch leaves.
	maxBatch = 512
	// checkpointFrames is the WAL growth, in frames since the last
	// checkpoint, that wakes the checkpointer after a batch. The WAL's pages
	// are charged to the process's memory: 1,024 frames keep it near 4 MiB
	// where 4,096 let it reach 17 MiB (bench/BENCH.md, S11).
	checkpointFrames = 1024
	// unlockedPasses bounds the passes that run beside the loops before the
	// checkpointer falls back to one that holds mu.
	unlockedPasses = 8
)

// engine owns the SQLite connection the loops use and the checkpointer's.
// Every call into the C engine but eng_checkpoint holds mu, whichever loop
// thread ends a batch; with one loop it is contended only by the
// checkpointer's last pass.
type engine struct {
	mu  sync.Mutex
	eng *C.eng
	ops []C.eng_op
	// view is what a batch reads back from the engine after eng_exec,
	// without a second cgo call.
	view  *C.eng_view
	start time.Time
	// mark is the WAL frame count the last checkpoint left in the database;
	// written under mu.
	mark int

	// ckpt wakes the checkpoint goroutine; stop and done end it.
	ckpt chan struct{}
	stop chan struct{}
	done chan struct{}

	stats batchStats
	info  dbInfo
}

// dbInfo is read once at startup, for the startup line.
type dbInfo struct {
	SQLite      string `json:"sqlite"`
	JournalMode string `json:"journal_mode"`
	LockingMode string `json:"locking_mode"`
	Synchronous string `json:"synchronous"`
	MmapSize    string `json:"mmap_size"`
	CacheSize   string `json:"cache_size"`
}

// batchStats are counters the State line reports; written under mu, read
// from the signal goroutine.
type batchStats struct {
	batches       atomic.Int64
	ops           atomic.Int64
	failed        atomic.Int64
	maxBatch      atomic.Int64
	execNanos     atomic.Int64
	checkpoints   atomic.Int64
	ckptPasses    atomic.Int64
	ckptNanos     atomic.Int64
	ckptMaxNanos  atomic.Int64
	blocked       atomic.Int64
	blockNanos    atomic.Int64
	blockMaxNanos atomic.Int64
	ckptLastLog   atomic.Int64
	ckptLastDone  atomic.Int64
	ckptErrors    atomic.Int64
	arenaCap      atomic.Int64
	execHist      histogram
	checkpointHst histogram
	blockHist     histogram
}

// openEngine opens the database and starts the checkpoint goroutine.
func openEngine(path string) (*engine, error) {
	e := &engine{
		start: time.Now(),
		ckpt:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	if err := e.open(path); err != nil {
		return nil, err
	}
	go e.checkpointer()
	return e, nil
}

func (e *engine) open(path string) error {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var errBuf [512]C.char
	eng := C.eng_open(cpath, maxBatch, &errBuf[0], C.int(len(errBuf)))
	if eng == nil {
		return errors.New(C.GoString(&errBuf[0]))
	}
	e.eng = eng
	e.ops = unsafe.Slice(C.eng_ops(eng), maxBatch)
	e.view = C.eng_view_of(eng)
	pragma := func(sql string) string {
		cs := C.CString(sql)
		defer C.free(unsafe.Pointer(cs))
		return C.GoString(C.eng_pragma_text(eng, cs))
	}
	e.info = dbInfo{
		SQLite:      C.GoString(C.eng_sqlite_version()),
		JournalMode: pragma("PRAGMA journal_mode"),
		LockingMode: pragma("PRAGMA locking_mode"),
		Synchronous: pragma("PRAGMA synchronous"),
		MmapSize:    pragma("PRAGMA mmap_size"),
		CacheSize:   pragma("PRAGMA cache_size"),
	}
	return nil
}

var engKind = [...]C.uint8_t{
	api.KindHealth:     C.ENG_HEALTH,
	api.KindFeed:       C.ENG_FEED,
	api.KindGetPost:    C.ENG_GET_POST,
	api.KindCreatePost: C.ENG_CREATE_POST,
	api.KindLikePost:   C.ENG_LIKE_POST,
}

// ExecBatch implements api.Executor: the loop's batch in one cgo call and one
// transaction (several past maxBatch). It returns after COMMIT, so no answer
// leaves before the writes it reports are durable. The bodies are copied out
// of the engine's arena into b.Out, which the loop owns: the arena is
// rewritten by the next call, possibly another loop's.
func (e *engine) ExecBatch(b *api.Batch) {
	b.Out = b.Out[:0]
	var t0, t1 time.Time
	var c1 time.Duration
	if trace != nil {
		t0 = time.Now()
	}
	e.mu.Lock()
	if trace != nil {
		t1 = time.Now()
		c1 = threadCPU()
	}
	for off := 0; off < len(b.Jobs); off += maxBatch {
		e.exec(b, b.Jobs[off:min(off+maxBatch, len(b.Jobs))])
	}
	due := e.due()
	e.mu.Unlock()
	if trace != nil {
		// The exec's CPU beside its wall time, from the thread it ran on:
		// the loop's, locked to it. Under the trace only, one getrusage
		// before and one after a batch.
		trace.batch(t0, len(b.Jobs), t1.Sub(t0), time.Since(t1), threadCPU()-c1)
	}
	if due {
		// The checkpoint runs off the loop, beside the next batches; only
		// its short last pass makes one of them wait on mu.
		select {
		case e.ckpt <- struct{}{}:
		default:
		}
	}
}

func (e *engine) exec(b *api.Batch, jobs []api.Job) {
	for i := range jobs {
		j := &jobs[i]
		op := &e.ops[i]
		*op = C.eng_op{kind: engKind[j.Kind]}
		switch j.Kind {
		case api.KindGetPost:
			op.arg1 = C.int64_t(j.PostID)
		case api.KindLikePost:
			op.arg1 = C.int64_t(j.PostID)
			op.arg2 = C.int64_t(j.UserID)
		case api.KindCreatePost:
			op.arg1 = C.int64_t(j.UserID)
			op.text_off, op.text_len = C.int32_t(j.TextOff), C.int32_t(j.TextLen)
			op.user_off, op.user_len = C.int32_t(j.UserOff), C.int32_t(j.UserLen)
		}
	}
	var inPtr *C.char
	if len(b.In) > 0 {
		inPtr = (*C.char)(unsafe.Pointer(&b.In[0]))
	}
	t0 := time.Now()
	uptime := int64(t0.Sub(e.start) / time.Second)
	failed := C.eng_exec(e.eng, C.int(len(jobs)), inPtr, C.int64_t(uptime))
	elapsed := time.Since(t0)

	// Grow b.Out once for the whole chunk, so the bodies taken below keep
	// pointing at the bytes they were copied into.
	total := 0
	for i := range jobs {
		total += int(e.ops[i].out_len)
	}
	b.Out = slices.Grow(b.Out, total)
	out := unsafe.Pointer(e.view.out)
	for i := range jobs {
		op := &e.ops[i]
		off := len(b.Out)
		b.Out = append(b.Out, unsafe.Slice((*byte)(unsafe.Add(out, int(op.out_off))), int(op.out_len))...)
		jobs[i].Status = int(op.status)
		jobs[i].Body = b.Out[off:len(b.Out):len(b.Out)]
	}

	s := &e.stats
	s.batches.Add(1)
	s.ops.Add(int64(len(jobs)))
	if failed != 0 {
		s.failed.Add(1)
	}
	if n := int64(len(jobs)); n > s.maxBatch.Load() {
		s.maxBatch.Store(n)
	}
	s.execNanos.Add(int64(elapsed))
	s.execHist.observe(elapsed)
}

// due reports whether the WAL grew by checkpointFrames since the last
// checkpoint; the caller holds mu. A WAL smaller than the mark was restarted
// by a writer, from frame zero.
func (e *engine) due() bool {
	frames := int(e.view.wal_frames)
	if frames < e.mark {
		e.mark = 0
	}
	return frames-e.mark >= checkpointFrames
}

// walFrames is the WAL's size in frames after the last commit.
func (e *engine) walFrames() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return int(C.eng_wal_frames(e.eng))
}

// checkpointer runs the checkpoints ExecBatch asks for. Each PASSIVE pass
// runs on the checkpointer's connection beside the loops, which keep reading
// and committing; it copies what the WAL held when it started, fsyncs
// included. The checkpoint is done once a pass ends with every frame copied
// and no commit since, checked under mu: the next write then restarts the
// WAL from frame zero instead of letting it grow. When commits keep landing
// during the passes, the last one runs under mu.
func (e *engine) checkpointer() {
	defer close(e.done)
	for {
		select {
		case <-e.ckpt:
			e.mu.Lock()
			due := e.due()
			e.mu.Unlock()
			if due {
				e.checkpoint()
			}
		case <-e.stop:
			return
		}
	}
}

// checkpoint runs one checkpoint: up to unlockedPasses passes beside the
// loops, then, if frames are still left, one under mu.
func (e *engine) checkpoint() {
	t0 := time.Now()
	var nlog, nckpt C.int
	rc := C.int(0)
	whole := false
	for pass := 0; pass < unlockedPasses && rc == 0 && !whole; pass++ {
		tp := time.Now()
		rc = checkpointRC(C.eng_checkpoint(e.eng, &nlog, &nckpt), nlog)
		if trace != nil {
			trace.pass(tp, time.Since(tp), false)
		}
		e.stats.ckptPasses.Add(1)
		e.mu.Lock()
		if whole = rc == 0 && int(C.eng_wal_frames(e.eng)) == int(nckpt); whole {
			e.mark = int(nckpt)
		}
		e.mu.Unlock()
	}
	elapsed := time.Since(t0)

	var blocked time.Duration
	if rc == 0 && !whole {
		e.mu.Lock()
		t1 := time.Now()
		rc = checkpointRC(C.eng_checkpoint(e.eng, &nlog, &nckpt), nlog)
		e.stats.ckptPasses.Add(1)
		if rc == 0 {
			e.mark = int(nckpt)
		}
		blocked = time.Since(t1)
		e.mu.Unlock()
		if trace != nil {
			trace.pass(t1, blocked, true)
		}
	}
	e.mu.Lock()
	var st C.eng_stats
	C.eng_get_stats(e.eng, &st)
	e.mu.Unlock()

	// A failing checkpoint leaves the WAL growing; say so once, and count
	// every occurrence for the State line.
	if rc != 0 && e.stats.ckptErrors.Add(1) == 1 {
		writeLine(map[string]any{"event": "checkpoint_error", "rc": int(rc)})
	}
	s := &e.stats
	s.checkpoints.Add(1)
	s.ckptLastLog.Store(int64(nlog))
	s.ckptLastDone.Store(int64(nckpt))
	s.arenaCap.Store(int64(st.arena_cap))
	s.ckptNanos.Add(int64(elapsed))
	if int64(elapsed) > s.ckptMaxNanos.Load() {
		s.ckptMaxNanos.Store(int64(elapsed))
	}
	s.checkpointHst.observe(elapsed)
	if blocked > 0 {
		s.blocked.Add(1)
		s.blockNanos.Add(int64(blocked))
		if int64(blocked) > s.blockMaxNanos.Load() {
			s.blockMaxNanos.Store(int64(blocked))
		}
		s.blockHist.observe(blocked)
	}
}

// checkpointRC is a checkpoint's outcome: SQLite's code, or SQLITE_ERROR
// for a pass that reported no WAL (nlog -1), which copied nothing.
func checkpointRC(rc, nlog C.int) C.int {
	if rc == 0 && nlog < 0 {
		return 1
	}
	return rc
}

// close stops the checkpoint goroutine and closes the database. The loops
// have stopped by then: the http component stops before this one.
func (e *engine) close() {
	close(e.stop)
	<-e.done
	e.mu.Lock()
	defer e.mu.Unlock()
	C.eng_close(e.eng)
	e.eng = nil
}

// exit stops the checkpointer, waits for the batch in flight, closes the
// database and ends the process with code, still holding mu: no loop batch
// runs on the closed engine, and none is left half done.
func (e *engine) exit(code int) {
	close(e.stop)
	<-e.done
	e.mu.Lock()
	C.eng_close(e.eng)
	os.Exit(code)
}

// statsSnapshot is the engine part of the State line.
type statsSnapshot struct {
	Batches         int64   `json:"batches"`
	Ops             int64   `json:"ops"`
	FailedBatches   int64   `json:"failed_batches"`
	MaxBatch        int64   `json:"max_batch"`
	ExecMicros      int64   `json:"exec_us_total"`
	ExecP99Micros   float64 `json:"exec_p99_us"`
	Checkpoints     int64   `json:"checkpoints"`
	CkptPasses      int64   `json:"checkpoint_passes"`
	CkptMicros      int64   `json:"checkpoint_us_total"`
	CkptMaxMicros   int64   `json:"checkpoint_max_us"`
	CkptP99Micros   float64 `json:"checkpoint_p99_us"`
	Blocked         int64   `json:"checkpoints_blocking"`
	BlockMicros     int64   `json:"checkpoint_block_us_total"`
	BlockMaxMicros  int64   `json:"checkpoint_block_max_us"`
	BlockP99Micros  float64 `json:"checkpoint_block_p99_us"`
	CkptLastFrames  int64   `json:"checkpoint_last_frames"`
	CkptLastWritten int64   `json:"checkpoint_last_written"`
	CkptErrors      int64   `json:"checkpoint_errors"`
	ArenaCap        int64   `json:"arena_cap"`
}

func (e *engine) snapshot() statsSnapshot {
	s := &e.stats
	return statsSnapshot{
		Batches:         s.batches.Load(),
		Ops:             s.ops.Load(),
		FailedBatches:   s.failed.Load(),
		MaxBatch:        s.maxBatch.Load(),
		ExecMicros:      s.execNanos.Load() / 1e3,
		ExecP99Micros:   s.execHist.quantile(0.99),
		Checkpoints:     s.checkpoints.Load(),
		CkptPasses:      s.ckptPasses.Load(),
		CkptMicros:      s.ckptNanos.Load() / 1e3,
		CkptMaxMicros:   s.ckptMaxNanos.Load() / 1e3,
		CkptP99Micros:   s.checkpointHst.quantile(0.99),
		Blocked:         s.blocked.Load(),
		BlockMicros:     s.blockNanos.Load() / 1e3,
		BlockMaxMicros:  s.blockMaxNanos.Load() / 1e3,
		BlockP99Micros:  s.blockHist.quantile(0.99),
		CkptLastFrames:  s.ckptLastLog.Load(),
		CkptLastWritten: s.ckptLastDone.Load(),
		CkptErrors:      s.ckptErrors.Load(),
		ArenaCap:        s.arenaCap.Load(),
	}
}
