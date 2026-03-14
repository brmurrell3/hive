// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package backend

import (
	"bufio"
	"encoding/binary"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
	"syscall"
)

// SystemMemoryMB returns the total physical system RAM in megabytes.
// It uses platform-specific methods:
//   - Linux:  parses /proc/meminfo for MemTotal
//   - macOS:  reads hw.memsize via syscall.Sysctl
//
// Returns 0 if detection fails so the caller can apply a fallback.
func SystemMemoryMB() int64 {
	switch goruntime.GOOS {
	case "linux":
		return linuxMemoryMB()
	case "darwin":
		return darwinMemoryMB()
	default:
		return 0
	}
}

// linuxMemoryMB parses /proc/meminfo to extract MemTotal in MB.
func linuxMemoryMB() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		// Format: "MemTotal:       16384000 kB"
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kB, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kB / 1024 // kB -> MB
	}
	return 0
}

// darwinMemoryMB reads hw.memsize via syscall.Sysctl to get total RAM in MB.
func darwinMemoryMB() int64 {
	val, err := syscall.Sysctl("hw.memsize")
	if err != nil || len(val) == 0 {
		return 0
	}
	// syscall.Sysctl returns a raw byte string; hw.memsize is a uint64 in
	// host byte order (little-endian on all supported Apple hardware).
	b := []byte(val)
	// The kernel may or may not include a trailing NUL byte.
	// Ensure we have at least 8 bytes for the uint64.
	if len(b) < 8 {
		return 0
	}
	memBytes := binary.LittleEndian.Uint64(b[:8])
	return int64(memBytes / (1024 * 1024))
}
