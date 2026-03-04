// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build !linux

package production

// checkProcComm is a no-op on non-Linux platforms where /proc is unavailable.
// Firecracker only runs on Linux, so PID reuse detection is not needed here.
// Returns true to indicate the process should be considered valid.
func checkProcComm(_ int) bool {
	return true
}
