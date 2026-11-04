//go:build !linux

package main

import "time"

// threadCPU is zero where getrusage has no per-thread figure: the trace's
// exec CPU columns then read zero.
func threadCPU() time.Duration { return 0 }
