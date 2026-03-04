package production

import (
	"os"
	"syscall"
)

// processExists checks whether a process with the given PID is alive by
// sending signal 0 (which checks for process existence without actually
// delivering a signal).
func processExists(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}

	// Signal 0 checks if the process exists without sending a real signal.
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}
