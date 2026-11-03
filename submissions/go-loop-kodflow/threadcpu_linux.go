package main

import (
	"syscall"
	"time"
)

// rusageThread is RUSAGE_THREAD (linux/resource.h): the calling thread
// alone, which the syscall package does not name.
const rusageThread = 1

// threadCPU is the CPU time, user and system, the calling thread has used:
// the scheduler's runtime split by getrusage(2). Without
// CONFIG_IRQ_TIME_ACCOUNTING, as on the bench box (Debian's 6.12 config and
// the 7.3.1 built from it), the interrupts and softirqs the kernel runs while
// the thread is on the CPU are charged to the thread and are in it: a wall
// time past it is then only time the thread was off the CPU, preempted by
// other tasks, not softirq taken from it. Executions under about 1 ms can
// read zero. One system call; zero if it fails.
func threadCPU() time.Duration {
	var ru syscall.Rusage
	if err := syscall.Getrusage(rusageThread, &ru); err != nil {
		return 0
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}
