// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build darwin

package main

import "golang.org/x/sys/unix"

// systemMemoryMB returns total system RAM in megabytes on macOS
// using the hw.memsize sysctl.
func systemMemoryMB() int64 {
	memBytes, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return 0
	}
	return int64(memBytes / (1024 * 1024))
}
