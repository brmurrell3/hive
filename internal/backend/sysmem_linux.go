// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build linux

package backend

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// SystemMemoryMB parses /proc/meminfo to extract MemTotal in MB.
func SystemMemoryMB() int64 {
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
