// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build !linux

package production

import (
	"runtime"
)

// systemMemoryUsage returns Go runtime memory usage as an approximation on
// non-Linux platforms where /proc/meminfo is unavailable. The percentage is
// based on the Go heap allocation relative to the system memory obtained from
// the runtime.
// Approximation: Uses Go runtime stats on non-Linux. Not actual system memory.
func systemMemoryUsage() (percent float64, totalBytes int64, usedBytes int64, err error) {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	// Sys is the total bytes of memory obtained from the OS by the Go runtime.
	// HeapInuse is memory in active heap spans. Using Alloc (bytes allocated
	// and still in use) gives the most meaningful "used" number.
	totalBytes = int64(memStats.Sys)
	usedBytes = int64(memStats.Alloc)

	if totalBytes > 0 {
		percent = float64(usedBytes) / float64(totalBytes) * 100.0
	}

	return percent, totalBytes, usedBytes, nil
}

// systemCPUUsage returns a rough CPU usage estimate on non-Linux platforms.
// Without /proc/stat, we use the ratio of active goroutines to GOMAXPROCS as
// a heuristic. This is not a true CPU measurement but provides a non-zero
// signal proportional to actual workload.
// Rough heuristic for non-Linux. Production deployments should use Linux for accurate metrics.
func systemCPUUsage() (percent float64, err error) {
	goroutines := runtime.NumGoroutine()
	maxProcs := runtime.GOMAXPROCS(0)

	// Each GOMAXPROCS slot can run one goroutine at a time. If we have more
	// goroutines than slots, we are likely CPU-bound. Scale so that
	// goroutines == maxProcs maps to ~50% usage (a reasonable midpoint since
	// many goroutines are blocked on I/O).
	if maxProcs > 0 {
		percent = float64(goroutines) / float64(maxProcs*2) * 100.0
		if percent > 100 {
			percent = 100
		}
	}

	return percent, nil
}

// systemCPUCount returns the number of logical CPUs available to the process.
func systemCPUCount() int {
	return runtime.NumCPU()
}
