// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build darwin

package backend

import (
	"encoding/binary"
	"syscall"
)

// SystemMemoryMB reads hw.memsize via syscall.Sysctl to get total RAM in MB.
func SystemMemoryMB() int64 {
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
