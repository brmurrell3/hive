// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package production

import (
	"errors"
	"os"
	"runtime"
	"syscall"
)

// processExists checks whether a process with the given PID is alive and is
// likely a Firecracker process (not a reused PID for an unrelated process).
//
// On Linux, it verifies the process command name via /proc/<pid>/comm to detect
// PID reuse. On other platforms (macOS, etc.), only signal-based existence
// checking is available, so PID reuse cannot be reliably detected. This is an
// acceptable limitation because Firecracker only runs on Linux in production.
func processExists(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	// Signal 0 checks if the process exists without sending a real signal.
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		// Process exists and we can signal it. On Linux, verify it is
		// actually a Firecracker process to guard against PID reuse.
		return isFirecrackerProcess(pid)
	}
	// EPERM means the process exists but we lack permission to signal it
	// (e.g., PID reused by a root-owned process). On Linux we can still
	// check /proc/<pid>/comm to verify it is Firecracker.
	if errors.Is(err, syscall.EPERM) {
		return isFirecrackerProcess(pid)
	}
	return false // ESRCH or other error means process is gone
}

// isFirecrackerProcess checks whether the process with the given PID is a
// Firecracker process. On Linux it reads /proc/<pid>/comm; on non-Linux
// platforms where Firecracker does not run, it returns true (assumes the
// process is valid since PID reuse detection is not critical there).
func isFirecrackerProcess(pid int) bool {
	if runtime.GOOS != "linux" {
		// On non-Linux platforms (macOS, etc.), Firecracker is not used in
		// production. We cannot check /proc, so assume the process is valid.
		// PID reuse is unlikely during the short crash recovery window.
		return true
	}

	return checkProcComm(pid)
}
