package production

import (
	"errors"
	"os"
	"syscall"
)

// processExists checks whether a process with the given PID is alive by
// sending signal 0 (which checks for process existence without actually
// delivering a signal).
// Returns true on EPERM (process exists but belongs to another user).
func processExists(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	// Signal 0 checks if the process exists without sending a real signal.
	err = proc.Signal(syscall.Signal(0))
	if err == nil {
		return true // process exists, we can signal it
	}
	// EPERM means the process exists but we lack permission to signal it
	// (e.g., PID reused by a root-owned process). Still alive.
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false // ESRCH or other error means process is gone
}
