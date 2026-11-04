package main

import (
	"math/bits"
	"sync/atomic"
	"time"
)

// histogram counts durations in power-of-two microsecond buckets: one writer,
// any number of readers, no locks. Quantiles are bucket upper bounds, which
// is precise enough to tell a 100 µs batch from a 10 ms stall.
type histogram struct {
	buckets [32]atomic.Int64
}

func (h *histogram) observe(d time.Duration) {
	us := uint64(d / time.Microsecond)
	h.buckets[min(bits.Len64(us), len(h.buckets)-1)].Add(1)
}

// quantile returns the upper bound, in microseconds, of the bucket holding
// quantile q, or 0 when nothing was observed.
func (h *histogram) quantile(q float64) float64 {
	var counts [32]int64
	var total int64
	for i := range h.buckets {
		counts[i] = h.buckets[i].Load()
		total += counts[i]
	}
	if total == 0 {
		return 0
	}
	rank := int64(q * float64(total))
	var seen int64
	for i, c := range counts {
		seen += c
		if seen > rank {
			return float64(uint64(1) << i)
		}
	}
	return float64(uint64(1) << (len(counts) - 1))
}
