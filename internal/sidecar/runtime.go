// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package sidecar

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

const (
	// sigTermGracePeriod is how long to wait for the process to exit after SIGTERM.
	sigTermGracePeriod = 15 * time.Second

	// sigKillWaitTimeout is how long to wait for the process to exit after SIGKILL.
	sigKillWaitTimeout = 5 * time.Second
)

// Runtime manages an agent's runtime process lifecycle.
type Runtime struct {
	command string
	args    []string
	workDir string
	cmd     *exec.Cmd
	running bool
	mu      sync.Mutex
	logger  *slog.Logger
	exitCh  chan error
	wg      sync.WaitGroup // tracks the monitor goroutine

	// EnvVars holds extra environment variables to inject into the runtime
	// process (e.g., secrets fetched from the control plane). Each entry
	// must be in "KEY=VALUE" format. These are appended to the current
	// process environment so the runtime inherits PATH and other basics.
	EnvVars []string
}

// NewRuntime creates a new Runtime that will execute the given command
// with the specified arguments and working directory. If command is empty,
// no process will be started (the runtime is a no-op).
func NewRuntime(command string, args []string, workDir string, logger *slog.Logger) *Runtime {
	return &Runtime{
		command: command,
		args:    args,
		workDir: workDir,
		logger:  logger,
		exitCh:  make(chan error, 1),
	}
}

// Start launches the runtime process. If no command was configured, it marks
// the runtime as running (healthy no-op) and returns nil.
func (r *Runtime) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.running {
		return fmt.Errorf("runtime already running")
	}

	// Create a fresh exit channel so that a Start() after Stop() does not
	// read stale values left over from the previous process lifecycle.
	r.exitCh = make(chan error, 1)

	// No command configured; treat as a healthy no-op runtime.
	if r.command == "" {
		r.logger.Info("no runtime command configured, running in no-op mode")
		r.running = true
		return nil
	}

	r.cmd = exec.Command(r.command, r.args...)
	r.cmd.Dir = r.workDir
	r.cmd.Stdout = os.Stdout
	r.cmd.Stderr = os.Stderr

	// Inject extra environment variables (e.g., secrets) into the runtime
	// process. We start from the current process environment so the runtime
	// inherits PATH and other essentials.
	if len(r.EnvVars) > 0 {
		r.cmd.Env = append(os.Environ(), r.EnvVars...)
	}

	if err := r.cmd.Start(); err != nil {
		return fmt.Errorf("starting runtime process: %w", err)
	}

	r.running = true
	r.logger.Info("runtime process started",
		"pid", r.cmd.Process.Pid,
		"command", r.command,
	)

	// Monitor the process in a background goroutine.
	r.wg.Add(1)
	go r.monitor()

	return nil
}

// monitor waits for the runtime process to exit and updates state accordingly.
func (r *Runtime) monitor() {
	defer r.wg.Done()
	err := r.cmd.Wait()

	r.mu.Lock()
	r.running = false
	r.mu.Unlock()

	if err != nil {
		r.logger.Warn("runtime process exited with error",
			"error", err,
		)
	} else {
		r.logger.Info("runtime process exited cleanly")
	}

	// Non-blocking send; the channel is buffered with capacity 1.
	select {
	case r.exitCh <- err:
	default:
	}
}

// Stop performs a graceful shutdown of the runtime process. It sends SIGTERM,
// waits up to 15 seconds, then sends SIGKILL if the process has not exited.
// For a no-op runtime (empty command), it simply marks the runtime as stopped.
func (r *Runtime) Stop() error {
	r.mu.Lock()
	if !r.running {
		r.mu.Unlock()
		return nil
	}

	// No-op runtime: mark as stopped and unblock any Wait() callers.
	if r.command == "" {
		r.running = false
		r.mu.Unlock()
		// Send nil to exitCh so that Wait() returns instead of blocking forever.
		select {
		case r.exitCh <- nil:
		default:
		}
		r.logger.Info("no-op runtime stopped")
		return nil
	}

	proc := r.cmd.Process
	r.mu.Unlock()

	if proc == nil {
		return nil
	}

	r.logger.Info("sending SIGTERM to runtime process", "pid", proc.Pid)
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		// Process may have already exited.
		r.logger.Warn("failed to send SIGTERM", "error", err)
		return nil
	}

	// Wait up to 15 seconds for graceful exit.
	select {
	case <-r.exitCh:
		r.logger.Info("runtime process exited after SIGTERM")
		return nil
	case <-time.After(sigTermGracePeriod):
		r.logger.Warn("runtime process did not exit after SIGTERM, sending SIGKILL",
			"pid", proc.Pid,
		)
	}

	if err := proc.Kill(); err != nil {
		r.logger.Warn("failed to send SIGKILL", "error", err)
		return fmt.Errorf("killing runtime process: %w", err)
	}

	// Wait for the process to actually exit after SIGKILL.
	select {
	case <-r.exitCh:
		r.logger.Info("runtime process exited after SIGKILL")
	case <-time.After(sigKillWaitTimeout):
		r.logger.Error("runtime process did not exit after SIGKILL")
		return fmt.Errorf("runtime process did not exit after SIGKILL")
	}

	return nil
}

// IsRunning returns whether the runtime process is currently running.
func (r *Runtime) IsRunning() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running
}

// Wait blocks until the runtime process exits and returns the exit error.
// For a no-op runtime, Wait blocks forever (until the caller's context is
// cancelled or Stop is called).
func (r *Runtime) Wait() error {
	return <-r.exitCh
}
