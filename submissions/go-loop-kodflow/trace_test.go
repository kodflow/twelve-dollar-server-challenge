package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kodflow/twelve-dollar-go-loop/internal/api"
)

// TestTraceWritesSlots runs batches with LOOP_TRACE's writer on and reads
// back a line with turns and operations.
func TestTraceWritesSlots(t *testing.T) {
	db, err := openEngine(freshSeed(t))
	if err != nil {
		t.Fatal(err)
	}
	defer db.close()
	path := filepath.Join(t.TempDir(), "trace.txt")
	if err := startTrace(path); err != nil {
		t.Fatal(err)
	}
	defer func() { trace = nil }()
	for range 50 {
		runBatch(db, api.Op{Kind: api.KindGetPost, PostID: 1}, api.Op{Kind: api.KindGetPost, PostID: 2})
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(2100 * time.Millisecond)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) < 3 || !strings.HasPrefix(lines[0], "t_ms turns") {
		t.Fatalf("trace: %q", b)
	}
	f := strings.Fields(lines[1])
	if len(f) != 19 || f[4] == "0" {
		t.Fatalf("slot line %q", lines[1])
	}
	// Without CONFIG_IRQ_TIME_ACCOUNTING the softirqs the kernel runs while
	// the thread is on the CPU are charged to it (threadCPU), so an exec's CPU
	// can pass its wall time, though not by more than a few ms; executions
	// under about 1 ms can read zero, so every slot may read none.
	const slack = 4000 // µs, one tick at HZ 250
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		cpu, _ := strconv.ParseInt(f[18], 10, 64)
		wall, _ := strconv.ParseInt(f[17], 10, 64)
		if cpu > wall+slack {
			t.Fatalf("slot %s: exec CPU %d µs past its wall time %d µs by more than %d", f[0], cpu, wall, slack)
		}
	}
}
