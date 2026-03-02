// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build linux

package production

import (
	"fmt"
	"os"
	"strings"
)

// checkProcComm reads /proc/<pid>/comm and returns true if the process name
// matches "firecracker". This guards against PID reuse: if the PID now belongs
// to a different program, we correctly report the Firecracker process as gone.
func checkProcComm(pid int) bool {
	commPath := fmt.Sprintf("/proc/%d/comm", pid)
	data, err := os.ReadFile(commPath)
	if err != nil {
		// Process may have exited between the signal check and this read.
		return false
	}
	comm := strings.TrimSpace(string(data))
	return comm == "firecracker"
}
