package main

import "testing"

// TestKernelCQBatch checks CQBatch's default by kernel release: 64 from
// 6.13 on, where the SDK does not sweep, the SDK's 512 before and for a
// release that does not parse.
func TestKernelCQBatch(t *testing.T) {
	for _, tc := range []struct {
		release string
		want    int
		source  string
	}{
		{"6.8.0-147-generic", sweepCQBatch, "kernel < 6.13"},
		{"6.12.107+deb13-amd64", sweepCQBatch, "kernel < 6.13"},
		{"6.13", defaultCQBatch, "kernel >= 6.13"},
		{"6.14.2-arch1-1", defaultCQBatch, "kernel >= 6.13"},
		{"7.3.1-s19", defaultCQBatch, "kernel >= 6.13"},
		{"5.15.0-1-generic", sweepCQBatch, "kernel < 6.13"},
		{"", sweepCQBatch, "kernel unknown"},
		{"linux", sweepCQBatch, "kernel unknown"},
		{"6.x", sweepCQBatch, "kernel unknown"},
	} {
		got, source := kernelCQBatch(tc.release)
		if got != tc.want || source != tc.source {
			t.Errorf("kernelCQBatch(%q) = %d, %q; want %d, %q", tc.release, got, source, tc.want, tc.source)
		}
	}
}
