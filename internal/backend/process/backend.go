// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package process implements the Backend interface for running agents as
// child processes. This is the simplest backend — no VMs, no containers,
// just exec.Command with environment injection and stdout/stderr capture.
package process

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/brmurrell3/hive/internal/backend"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
)

// Backend implements backend.Backend for local processes.
type Backend struct {
	logger      *slog.Logger
	store       *state.Store
	clusterRoot string // absolute path to the cluster root directory
	mu          sync.RWMutex
	instances   map[string]*processInstance
}

// maxBufSize is the maximum number of bytes retained per output buffer (10 MB).
// Once the limit is reached, additional writes are silently dropped.
const maxBufSize = 10 * 1024 * 1024

// safeBuf is a thread-safe bytes.Buffer with a maximum size limit.
// It implements io.Writer so it can be used directly as cmd.Stdout/Stderr.
type safeBuf struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	truncated bool // set when data is dropped due to maxBufSize
}

// truncationMsg is appended to output when truncation has occurred.
const truncationMsg = "\n... output truncated (10MB limit) ...\n"

// Write implements io.Writer. This intentionally violates the io.Writer
// contract by always returning len(p), nil even when data is truncated.
// This is necessary because safeBuf is used as cmd.Stdout/Stderr —
// returning n < len(p) or a non-nil error would cause exec.Command to
// abort the child process prematurely. Truncation beyond maxBufSize is
// an acceptable trade-off for keeping the process alive.
func (s *safeBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p) // always report full write to caller
	remaining := maxBufSize - s.buf.Len()
	if remaining <= 0 {
		// Buffer full — record truncation and discard.
		s.truncated = true
		return n, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		s.truncated = true
	}
	// Ignore the bytes-written count from the underlying buffer; we
	// always report n (original len) to avoid aborting the child process.
	_, _ = s.buf.Write(p)
	return n, nil
}

// Bytes returns a copy of the buffered data, safe for concurrent use.
// If truncation occurred, a truncation indicator is appended to the output.
func (s *safeBuf) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.truncated {
		cp := make([]byte, s.buf.Len()+len(truncationMsg))
		copy(cp, s.buf.Bytes())
		copy(cp[s.buf.Len():], truncationMsg)
		return cp
	}
	cp := make([]byte, s.buf.Len())
	copy(cp, s.buf.Bytes())
	return cp
}

type processInstance struct {
	id            string
	agentID       string
	cmd           *exec.Cmd
	stdout        *safeBuf
	stderr        *safeBuf
	cancel        context.CancelFunc
	done          chan struct{}
	closeOnce     sync.Once  // BE-H2: prevents double-close of done channel
	mu            sync.Mutex // guards started and stopped flags
	started       bool       // BE-H8: prevents double-Start
	stopped       bool       // BE-H7: prevents Start after Stop
	err           error
	workspacePath string // set for OpenClaw agents; cleaned up in Destroy
	gatewayPort   int    // BE-C1: OpenClaw gateway port; released in Destroy
}

func (i *processInstance) ID() string      { return i.id }
func (i *processInstance) AgentID() string { return i.agentID }
func (i *processInstance) Backend() string { return "process" }

// New creates a new process backend. The store parameter is optional (may be
// nil) for backward compatibility; when non-nil the backend writes agent
// status transitions into the state store. The clusterRoot parameter is
// optional (may be empty) and is required for OpenClaw runtime support;
// when set it provides the base path for agent source directories and
// workspace state.
func New(logger *slog.Logger, store *state.Store, clusterRoot ...string) *Backend {
	root := ""
	if len(clusterRoot) > 0 {
		root = clusterRoot[0]
	}
	return &Backend{
		logger:      logger,
		store:       store,
		clusterRoot: root,
		instances:   make(map[string]*processInstance),
	}
}

func (b *Backend) Name() string { return "process" }

func (b *Backend) Capabilities() backend.BackendCaps {
	return backend.BackendCaps{
		Isolation:              "process",
		SupportsSnapshots:      false,
		SupportsNetworkPolicy:  false,
		SupportsMounts:         false,
		SupportsResourceLimits: false,
	}
}

func (b *Backend) Create(ctx context.Context, spec *types.AgentManifest) (backend.Instance, error) {
	agentID := spec.Metadata.ID

	var cmdName string
	var cmdArgs []string
	var openclawPort int
	var wsPath string // workspace directory to clean up on Destroy (OpenClaw only)

	if isOpenClawRuntime(spec) {
		// OpenClaw runtime path: use the openclaw binary instead of
		// the manifest's runtime.command.
		if b.clusterRoot == "" {
			return nil, fmt.Errorf("agent %q uses openclaw runtime but process backend has no cluster root configured", agentID)
		}

		binaryPath, err := findOpenClawBinary()
		if err != nil {
			return nil, fmt.Errorf("agent %q: %w", agentID, err)
		}

		workspacePath, port, err := prepareOpenClawWorkspace(b.clusterRoot, agentID, spec)
		if err != nil {
			return nil, fmt.Errorf("agent %q: %w", agentID, err)
		}

		configPath := filepath.Join(workspacePath, "openclaw.json")
		cmdName = binaryPath
		cmdArgs = []string{"--config", configPath}
		openclawPort = port
		wsPath = workspacePath

		b.logger.Info("openclaw workspace prepared",
			"agent_id", agentID,
			"workspace", workspacePath,
			"gateway_port", port,
		)
	}

	// BE-C2: Deferred cleanup of port and workspace if Create fails after
	// prepareOpenClawWorkspace has succeeded. The success flag is set just
	// before the successful return so that early returns trigger cleanup.
	success := false
	defer func() {
		if !success && openclawPort > 0 {
			releaseGatewayPort(openclawPort)
			if wsPath != "" {
				os.RemoveAll(wsPath) //nolint:errcheck // best-effort cleanup on failure path
			}
		}
	}()

	if !isOpenClawRuntime(spec) {
		// Standard process runtime path.
		runtimeCmd := spec.Spec.Runtime.Command
		if runtimeCmd == "" {
			return nil, fmt.Errorf("agent %q has no runtime command specified", agentID)
		}

		parts := strings.Fields(runtimeCmd)
		cmdName = parts[0]
		if len(parts) > 1 {
			cmdArgs = parts[1:]
		}
	}

	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, cmdName, cmdArgs...)

	// Build environment from a minimal allow-list of parent variables
	// instead of inheriting the full parent environment, which could
	// leak secrets (BE-H2).
	env := make([]string, 0, 16)
	for _, key := range []string{"PATH", "HOME", "USER", "TMPDIR", "LANG", "TERM"} {
		if v := os.Getenv(key); v != "" {
			env = append(env, fmt.Sprintf("%s=%s", key, v))
		}
	}

	// Add model env vars BEFORE HIVE_* assignments so that a malicious
	// model config cannot override framework-critical variables (BE-H1).
	// A denylist prevents injection of HIVE_*, LD_*, DYLD_*, PATH,
	// HOME, and SHELL keys.
	for k, v := range spec.Spec.Runtime.Model.Env {
		if isEnvVarDenied(k) {
			b.logger.Warn("model env var denied by denylist",
				"agent_id", agentID, "key", k)
			continue
		}
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	// HIVE_* assignments come after model env vars so they cannot be
	// overridden.
	env = append(env,
		fmt.Sprintf("HIVE_AGENT_ID=%s", agentID),
		fmt.Sprintf("HIVE_TEAM=%s", spec.Metadata.Team),
		fmt.Sprintf("HIVE_TEAM_ID=%s", spec.Metadata.Team),
	)

	// Add sidecar and callback env vars if available.
	if v := os.Getenv("HIVE_NATS_URL"); v != "" {
		env = append(env, fmt.Sprintf("HIVE_NATS_URL=%s", v))
	}
	if v := os.Getenv("HIVE_NATS_TOKEN"); v != "" {
		env = append(env, fmt.Sprintf("HIVE_NATS_TOKEN=%s", v))
	}
	if v := os.Getenv("HIVE_SIDECAR_URL"); v != "" {
		env = append(env, fmt.Sprintf("HIVE_SIDECAR_URL=%s", v))
	}
	if v := os.Getenv("HIVE_CALLBACK_PORT"); v != "" {
		env = append(env, fmt.Sprintf("HIVE_CALLBACK_PORT=%s", v))
	}

	// Add OpenClaw-specific env vars.
	// NOTE: The sidecar bridges capability invocations to the OpenClaw
	// gateway via HTTP at this port. That integration is handled separately
	// in the sidecar package.
	if openclawPort > 0 {
		env = append(env, fmt.Sprintf("HIVE_OPENCLAW_PORT=%d", openclawPort))
	}

	cmd.Env = env

	stdout := &safeBuf{}
	stderr := &safeBuf{}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	// Set PENDING state before creating the instance.
	if b.store != nil {
		if err := b.store.SetAgent(&state.AgentState{
			ID:     agentID,
			Status: state.AgentStatusPending,
		}); err != nil {
			cancel()
			return nil, fmt.Errorf("setting agent %q to PENDING: %w", agentID, err)
		}
	}

	inst := &processInstance{
		id:            agentID,
		agentID:       agentID,
		cmd:           cmd,
		stdout:        stdout,
		stderr:        stderr,
		cancel:        cancel,
		done:          make(chan struct{}),
		workspacePath: wsPath,
		gatewayPort:   openclawPort, // BE-C1: store port for release in Destroy
	}

	// Transition to CREATING state.
	if b.store != nil {
		if err := b.store.SetAgent(&state.AgentState{
			ID:     agentID,
			Status: state.AgentStatusCreating,
		}); err != nil {
			cancel()
			return nil, fmt.Errorf("setting agent %q to CREATING: %w", agentID, err)
		}
	}

	b.mu.Lock()
	if _, exists := b.instances[agentID]; exists {
		b.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("agent %q already exists; concurrent Create() detected", agentID)
	}
	b.instances[agentID] = inst
	b.mu.Unlock()

	b.logger.Info("process instance created", "agent_id", agentID, "cmd", cmdName)
	success = true // BE-C2: disarm deferred cleanup
	return inst, nil
}

func (b *Backend) Start(ctx context.Context, id string) error {
	b.mu.RLock()
	inst, ok := b.instances[id]
	b.mu.RUnlock()

	if !ok {
		return fmt.Errorf("instance %q not found", id)
	}

	// BE-H7/BE-H8: Prevent double-start and start-after-stop races.
	inst.mu.Lock()
	if inst.stopped {
		inst.mu.Unlock()
		return fmt.Errorf("instance %q has been stopped; cannot start", id)
	}
	if inst.started {
		inst.mu.Unlock()
		return fmt.Errorf("instance %q already started; cannot start twice", id)
	}
	inst.started = true
	inst.mu.Unlock()

	// Transition to STARTING state.
	if b.store != nil {
		if err := b.store.SetAgent(&state.AgentState{
			ID:     id,
			Status: state.AgentStatusStarting,
		}); err != nil {
			return fmt.Errorf("setting agent %q to STARTING: %w", id, err)
		}
	}

	if err := inst.cmd.Start(); err != nil {
		if b.store != nil {
			if sErr := b.store.SetAgent(&state.AgentState{
				ID:     id,
				Status: state.AgentStatusFailed,
			}); sErr != nil {
				b.logger.Error("failed to set agent status to FAILED after start error", "agent_id", id, "error", sErr)
			}
		}
		return fmt.Errorf("starting process: %w", err)
	}

	if b.store != nil {
		if err := b.store.SetAgent(&state.AgentState{
			ID:     id,
			Status: state.AgentStatusRunning,
		}); err != nil {
			// Process is running but we failed to record that fact.
			// Kill the process to avoid a zombie that the caller
			// doesn't know about.
			b.logger.Error("store write failed after process started; killing process to avoid zombie",
				"agent_id", id, "error", err)
			inst.cancel()
			// Wait for the process to exit so we don't leak it.
			_ = inst.cmd.Wait()
			inst.closeOnce.Do(func() { close(inst.done) })
			return fmt.Errorf("setting agent %q to RUNNING: %w", id, err)
		}
	}

	// Wait in background.
	go func() {
		inst.err = inst.cmd.Wait()
		inst.closeOnce.Do(func() { close(inst.done) })
		b.logger.Info("process exited", "agent_id", id, "error", inst.err)
	}()

	b.logger.Info("process started", "agent_id", id, "pid", inst.cmd.Process.Pid)
	return nil
}

func (b *Backend) Stop(ctx context.Context, id string) error {
	b.mu.RLock()
	inst, ok := b.instances[id]
	b.mu.RUnlock()

	if !ok {
		return fmt.Errorf("instance %q not found", id)
	}

	// BE-H7: Mark the instance as stopped so that a concurrent or
	// subsequent Start call is rejected.
	inst.mu.Lock()
	inst.stopped = true
	inst.mu.Unlock()

	// Transition to STOPPING state.
	if b.store != nil {
		if err := b.store.SetAgent(&state.AgentState{
			ID:     id,
			Status: state.AgentStatusStopping,
		}); err != nil {
			b.logger.Error("failed to set agent status to STOPPING", "agent_id", id, "error", err)
		}
	}

	// If the process was never started (cmd.Process is nil), the Wait
	// goroutine was never launched and inst.done will never close. In that
	// case, cancel the context and close done so callers don't block.
	// BE-H2: Use closeOnce to prevent double-close.
	if inst.cmd.Process == nil {
		inst.cancel()
		inst.closeOnce.Do(func() { close(inst.done) })
	} else {
		// Send SIGTERM first for graceful shutdown.
		inst.cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck // best-effort graceful shutdown signal

		// Wait with timeout, then force kill. Also respect the caller's context.
		select {
		case <-inst.done:
			// Process exited gracefully.
		case <-ctx.Done():
			inst.cancel() // Context cancelled — force kill immediately.
			<-inst.done
		case <-time.After(10 * time.Second):
			inst.cancel() // Force kill via context cancellation.
			<-inst.done
		}
	}

	if b.store != nil {
		if err := b.store.SetAgent(&state.AgentState{
			ID:     id,
			Status: state.AgentStatusStopped,
		}); err != nil {
			b.logger.Error("failed to set agent status to STOPPED", "agent_id", id, "error", err)
		}
	}

	b.logger.Info("process stopped", "agent_id", id)
	return nil
}

func (b *Backend) Destroy(ctx context.Context, id string) error {
	if err := b.Stop(ctx, id); err != nil {
		b.logger.Warn("error stopping during destroy", "agent_id", id, "error", err)
	}

	b.mu.Lock()
	inst, exists := b.instances[id]
	if !exists {
		b.mu.Unlock()
		return fmt.Errorf("instance %q not found; nothing to destroy", id)
	}
	delete(b.instances, id)
	b.mu.Unlock()

	// BE-C1: Release the OpenClaw gateway port back to the pool.
	if inst.gatewayPort > 0 {
		releaseGatewayPort(inst.gatewayPort)
	}

	// Clean up the OpenClaw workspace directory if one was created (BE-H5).
	if inst.workspacePath != "" {
		if err := os.RemoveAll(inst.workspacePath); err != nil {
			b.logger.Warn("failed to remove workspace directory",
				"agent_id", id, "path", inst.workspacePath, "error", err)
		}
	}

	if b.store != nil {
		if err := b.store.RemoveAgent(id); err != nil {
			b.logger.Error("failed to remove agent from store", "agent_id", id, "error", err)
		}
	}

	return nil
}

func (b *Backend) Status(ctx context.Context, id string) (backend.InstanceStatus, error) {
	b.mu.RLock()
	inst, ok := b.instances[id]
	b.mu.RUnlock()

	if !ok {
		return backend.InstanceStatus{State: "unknown"}, nil
	}

	select {
	case <-inst.done:
		exitCode := 0
		if inst.cmd.ProcessState != nil {
			exitCode = inst.cmd.ProcessState.ExitCode()
		}
		state := "stopped"
		if exitCode != 0 {
			state = "failed"
		}
		return backend.InstanceStatus{
			State:    state,
			ExitCode: exitCode,
		}, nil
	default:
		return backend.InstanceStatus{State: "running"}, nil
	}
}

func (b *Backend) Logs(ctx context.Context, id string, opts backend.LogOpts) (io.ReadCloser, error) {
	b.mu.RLock()
	inst, ok := b.instances[id]
	b.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("instance %q not found", id)
	}

	// Use io.MultiReader to avoid allocating a single combined buffer
	// that could reach 2 * maxBufSize (up to 20 MB) (BE-H6).
	combined := io.MultiReader(
		bytes.NewReader(inst.stdout.Bytes()),
		bytes.NewReader(inst.stderr.Bytes()),
	)
	return io.NopCloser(combined), nil
}

func (b *Backend) Available() backend.Resources {
	cpuCount := goruntime.NumCPU()

	// Detect actual system RAM using platform-specific methods.
	memTotal := backend.SystemMemoryMB()
	if memTotal < 256 {
		memTotal = 8192 // fallback if detection fails or returns unreasonably low value
	}

	return backend.Resources{
		MemoryMB: memTotal,
		VCPUs:    cpuCount,
		DiskMB:   51200, // disk is harder to detect cross-platform, keep reasonable default
	}
}

func (b *Backend) Allocated() backend.Resources {
	b.mu.RLock()
	defer b.mu.RUnlock()

	count := int64(0)
	for _, inst := range b.instances {
		select {
		case <-inst.done:
		default:
			count++
		}
	}
	return backend.Resources{
		MemoryMB: count * 256,
		VCPUs:    int(count),
	}
}

var _ backend.Backend = (*Backend)(nil)
