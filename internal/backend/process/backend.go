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
	logger    *slog.Logger
	store     *state.Store
	mu        sync.RWMutex
	instances map[string]*processInstance
}

type processInstance struct {
	id      string
	agentID string
	cmd     *exec.Cmd
	stdout  *bytes.Buffer
	stderr  *bytes.Buffer
	cancel  context.CancelFunc
	done    chan struct{}
	err     error
}

func (i *processInstance) ID() string      { return i.id }
func (i *processInstance) AgentID() string { return i.agentID }
func (i *processInstance) Backend() string { return "process" }

// New creates a new process backend. The store parameter is optional (may be
// nil) for backward compatibility; when non-nil the backend writes agent
// status transitions into the state store.
func New(logger *slog.Logger, store *state.Store) *Backend {
	return &Backend{
		logger:    logger,
		store:     store,
		instances: make(map[string]*processInstance),
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
	runtimeCmd := spec.Spec.Runtime.Command
	if runtimeCmd == "" {
		return nil, fmt.Errorf("agent %q has no runtime command specified", agentID)
	}

	parts := strings.Fields(runtimeCmd)
	cmdName := parts[0]
	var cmdArgs []string
	if len(parts) > 1 {
		cmdArgs = parts[1:]
	}

	procCtx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(procCtx, cmdName, cmdArgs...)

	// Build environment.
	cmd.Env = os.Environ()
	cmd.Env = append(cmd.Env,
		fmt.Sprintf("HIVE_AGENT_ID=%s", agentID),
		fmt.Sprintf("HIVE_TEAM=%s", spec.Metadata.Team),
		fmt.Sprintf("HIVE_TEAM_ID=%s", spec.Metadata.Team),
	)

	// Add sidecar and callback env vars if available.
	if v := os.Getenv("HIVE_NATS_URL"); v != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("HIVE_NATS_URL=%s", v))
	}
	if v := os.Getenv("HIVE_NATS_TOKEN"); v != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("HIVE_NATS_TOKEN=%s", v))
	}
	if v := os.Getenv("HIVE_SIDECAR_URL"); v != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("HIVE_SIDECAR_URL=%s", v))
	}
	if v := os.Getenv("HIVE_CALLBACK_PORT"); v != "" {
		cmd.Env = append(cmd.Env, fmt.Sprintf("HIVE_CALLBACK_PORT=%s", v))
	}

	// Add model env vars.
	for k, v := range spec.Spec.Runtime.Model.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

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
		id:      agentID,
		agentID: agentID,
		cmd:     cmd,
		stdout:  &stdout,
		stderr:  &stderr,
		cancel:  cancel,
		done:    make(chan struct{}),
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
	b.instances[agentID] = inst
	b.mu.Unlock()

	b.logger.Info("process instance created", "agent_id", agentID, "cmd", runtimeCmd)
	return inst, nil
}

func (b *Backend) Start(ctx context.Context, id string) error {
	b.mu.RLock()
	inst, ok := b.instances[id]
	b.mu.RUnlock()

	if !ok {
		return fmt.Errorf("instance %q not found", id)
	}

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
			return fmt.Errorf("setting agent %q to RUNNING: %w", id, err)
		}
	}

	// Wait in background.
	go func() {
		inst.err = inst.cmd.Wait()
		close(inst.done)
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

	// Transition to STOPPING state.
	if b.store != nil {
		if err := b.store.SetAgent(&state.AgentState{
			ID:     id,
			Status: state.AgentStatusStopping,
		}); err != nil {
			b.logger.Error("failed to set agent status to STOPPING", "agent_id", id, "error", err)
		}
	}

	// Send SIGTERM first for graceful shutdown.
	if inst.cmd.Process != nil {
		inst.cmd.Process.Signal(syscall.SIGTERM) //nolint:errcheck // best-effort graceful shutdown signal
	}

	// Wait with timeout, then force kill.
	select {
	case <-inst.done:
		// Process exited gracefully.
	case <-time.After(10 * time.Second):
		inst.cancel() // Force kill via context cancellation.
		<-inst.done
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
	delete(b.instances, id)
	b.mu.Unlock()

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

	combined := append(inst.stdout.Bytes(), inst.stderr.Bytes()...)
	return io.NopCloser(bytes.NewReader(combined)), nil
}

func (b *Backend) Available() backend.Resources {
	// Use runtime to get actual system info.
	// Try to read actual CPU count
	cpuCount := goruntime.NumCPU()

	// For memory, use a simple cross-platform approach
	var m goruntime.MemStats
	goruntime.ReadMemStats(&m)
	memTotal := int64(m.Sys / (1024 * 1024))
	if memTotal < 256 {
		memTotal = 8192 // fallback if reading fails
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
