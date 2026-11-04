package main

import (
	"bufio"
	"fmt"
	"math/bits"
	"os"
	"sync"
	"time"
)

// traceSlotWidth is the width of one trace line.
const traceSlotWidth = 100 * time.Millisecond

// traceBuckets is the log2 histogram's width: bucket i counts durations of
// [2^i, 2^(i+1)) µs, the last one everything longer.
const traceBuckets = 24

// traceSlot aggregates what the loop and the checkpointer did in one
// traceSlotWidth: the loop's turns (the time between two ExecBatch calls),
// the operations per batch, the batch's execution — wall time and the CPU
// the loop's thread used for it — and its wait for mu, and the checkpoint's
// passes, the last one's under mu apart.
type traceSlot struct {
	start         int64 // unix ms
	turns         int64
	turnMax       int64 // µs
	turnHist      [traceBuckets]int64
	ops, opsMax   int64
	execMax       int64 // µs
	execMaxCPU    int64 // µs, the CPU the longest exec used
	execCPUSum    int64 // µs, every exec's CPU
	execSum       int64 // µs, every exec's wall time
	waitSum       int64 // µs, the loop's wait for mu
	waitMax       int64
	passes        int64
	passMax       int64 // µs, a pass beside the loop
	blocked       int64 // µs, the last pass under mu
	blockedMax    int64
	blockedPasses int64
}

// loopTrace writes one line per traceSlotWidth to a file, for the S22 and
// S23 diagnosis of the p99 cliff: LOOP_TRACE names the file, empty turns
// it off. Only the loop's goroutine and the checkpointer write to it, under
// mu; a writer goroutine flushes the slots that are over once a second.
type loopTrace struct {
	mu       sync.Mutex
	slots    map[int64]*traceSlot
	lastTurn time.Time
	// state reads the server's batch p99 and batches cut, once a second.
	state func() (time.Duration, uint64)
	w     *bufio.Writer
	f     *os.File
}

// trace is the process's trace, nil when LOOP_TRACE is unset.
var trace *loopTrace

// startTrace opens LOOP_TRACE's file and starts its writer, or does nothing
// when path is empty.
func startTrace(path string) error {
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("LOOP_TRACE: %w", err)
	}
	t := &loopTrace{slots: map[int64]*traceSlot{}, f: f, w: bufio.NewWriter(f)}
	fmt.Fprintln(t.w, "t_ms turns turn_max_us turn_p99_us ops ops_max exec_max_us wait_sum_us wait_max_us passes pass_max_us blocked_passes blocked_us blocked_max_us sdk_batch_p99_us sdk_batches_cut exec_max_cpu_us exec_sum_us exec_cpu_sum_us")
	trace = t
	go t.writer()
	return nil
}

// watch sets what the writer reads from the server once a second; a nil
// trace ignores it.
func (t *loopTrace) watch(f func() (time.Duration, uint64)) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.state = f
	t.mu.Unlock()
}

// slot returns the slot of now; the caller holds mu.
func (t *loopTrace) slot(now time.Time) *traceSlot {
	k := now.UnixMilli() / traceSlotWidth.Milliseconds()
	s := t.slots[k]
	if s == nil {
		s = &traceSlot{start: k * traceSlotWidth.Milliseconds()}
		t.slots[k] = s
	}
	return s
}

// batch records one ExecBatch: begun at start, with ops operations, a wait
// of wait for mu and exec under it, of which the thread used cpu.
func (t *loopTrace) batch(start time.Time, ops int, wait, exec, cpu time.Duration) {
	t.mu.Lock()
	s := t.slot(start)
	if !t.lastTurn.IsZero() {
		turn := start.Sub(t.lastTurn).Microseconds()
		s.turns++
		s.turnMax = max(s.turnMax, turn)
		s.turnHist[min(bits.Len64(uint64(max(turn, 1)))-1, traceBuckets-1)]++
	}
	t.lastTurn = start
	s.ops += int64(ops)
	s.opsMax = max(s.opsMax, int64(ops))
	if exec.Microseconds() >= s.execMax {
		s.execMax, s.execMaxCPU = exec.Microseconds(), cpu.Microseconds()
	}
	s.execSum += exec.Microseconds()
	s.execCPUSum += cpu.Microseconds()
	s.waitSum += wait.Microseconds()
	s.waitMax = max(s.waitMax, wait.Microseconds())
	t.mu.Unlock()
}

// pass records one checkpoint pass begun at start and lasting d, under mu
// when underMu.
func (t *loopTrace) pass(start time.Time, d time.Duration, underMu bool) {
	t.mu.Lock()
	s := t.slot(start)
	if underMu {
		s.blockedPasses++
		s.blocked += d.Microseconds()
		s.blockedMax = max(s.blockedMax, d.Microseconds())
	} else {
		s.passes++
		s.passMax = max(s.passMax, d.Microseconds())
	}
	t.mu.Unlock()
}

// writer flushes, once a second, every slot that ended before the current
// one, in order.
func (t *loopTrace) writer() {
	for range time.Tick(time.Second) {
		cur := time.Now().UnixMilli() / traceSlotWidth.Milliseconds()
		t.mu.Lock()
		var done []*traceSlot
		for k, s := range t.slots {
			if k < cur {
				done = append(done, s)
				delete(t.slots, k)
			}
		}
		state := t.state
		t.mu.Unlock()
		var p99 time.Duration
		var cut uint64
		if state != nil {
			p99, cut = state()
		}
		sortSlots(done)
		for _, s := range done {
			fmt.Fprintf(t.w, "%d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d %d\n", s.start, s.turns, s.turnMax, s.p99(),
				s.ops, s.opsMax, s.execMax, s.waitSum, s.waitMax, s.passes, s.passMax, s.blockedPasses, s.blocked, s.blockedMax,
				p99.Microseconds(), cut, s.execMaxCPU, s.execSum, s.execCPUSum)
		}
		_ = t.w.Flush()
	}
}

// p99 is the upper bound, in µs, of the histogram bucket holding the
// slot's 99th-percentile turn.
func (s *traceSlot) p99() int64 {
	if s.turns == 0 {
		return 0
	}
	want := s.turns - s.turns/100
	var n int64
	for i, c := range s.turnHist {
		n += c
		if n >= want {
			return 1 << (i + 1)
		}
	}
	return s.turnMax
}

// sortSlots orders slots by start, a handful a second.
func sortSlots(s []*traceSlot) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j].start < s[j-1].start; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
