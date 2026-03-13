// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package firecracker implements the Backend interface using Firecracker VMs.
// It wraps the existing vm.Manager to provide the higher-level Backend
// abstraction while preserving all existing Firecracker functionality.
package firecracker

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/brmurrell3/hive/internal/backend"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/brmurrell3/hive/internal/vm"
)

// Backend implements the backend.Backend interface using Firecracker VMs.
type Backend struct {
	vmMgr     *vm.Manager
	store     *state.Store
	logger    *slog.Logger
	mu        sync.RWMutex
	instances map[string]*instance
}

type instance struct {
	id      string
	agentID string
}

func (i *instance) ID() string      { return i.id }
func (i *instance) AgentID() string { return i.agentID }
func (i *instance) Backend() string { return "firecracker" }

// New creates a new Firecracker backend.
func New(vmMgr *vm.Manager, store *state.Store, logger *slog.Logger) *Backend {
	return &Backend{
		vmMgr:     vmMgr,
		store:     store,
		logger:    logger,
		instances: make(map[string]*instance),
	}
}

func (b *Backend) Name() string { return "firecracker" }

func (b *Backend) Capabilities() backend.BackendCaps {
	return backend.BackendCaps{
		Isolation:              "vm",
		SupportsSnapshots:      true,
		SupportsNetworkPolicy:  true,
		SupportsMounts:         true,
		SupportsResourceLimits: true,
	}
}

func (b *Backend) Create(ctx context.Context, spec *types.AgentManifest) (backend.Instance, error) {
	agentID := spec.Metadata.ID

	if err := b.vmMgr.StartAgent(ctx, spec); err != nil {
		return nil, fmt.Errorf("creating firecracker VM: %w", err)
	}

	inst := &instance{id: agentID, agentID: agentID}
	b.mu.Lock()
	b.instances[agentID] = inst
	b.mu.Unlock()

	return inst, nil
}

func (b *Backend) Start(ctx context.Context, id string) error {
	// Firecracker VMs are started during Create (StartAgent does both).
	return nil
}

func (b *Backend) Stop(ctx context.Context, id string) error {
	return b.vmMgr.StopAgent(ctx, id)
}

func (b *Backend) Destroy(ctx context.Context, id string) error {
	err := b.vmMgr.DestroyAgent(id)

	b.mu.Lock()
	delete(b.instances, id)
	b.mu.Unlock()

	return err
}

func (b *Backend) Status(ctx context.Context, id string) (backend.InstanceStatus, error) {
	agent := b.store.GetAgent(id)
	if agent == nil {
		return backend.InstanceStatus{}, fmt.Errorf("agent %q not found", id)
	}

	return backend.InstanceStatus{
		State: string(agent.Status),
	}, nil
}

func (b *Backend) Logs(ctx context.Context, id string, opts backend.LogOpts) (io.ReadCloser, error) {
	return nil, fmt.Errorf("logs via backend not yet implemented for firecracker; use hivectl agents logs")
}

func (b *Backend) Available() backend.Resources {
	// Estimate from system resources — this is approximate.
	return backend.Resources{
		MemoryMB: 4096,
		VCPUs:    4,
		DiskMB:   10240,
	}
}

func (b *Backend) Allocated() backend.Resources {
	var total backend.Resources
	for _, agent := range b.store.AllAgents() {
		if agent.Status == state.AgentStatusRunning ||
			agent.Status == state.AgentStatusStarting ||
			agent.Status == state.AgentStatusCreating {
			// Use actual resource values from agent state (set during StartAgent).
			// MemoryBytes is stored in bytes; convert to MB for the Resources struct.
			if agent.MemoryBytes > 0 {
				total.MemoryMB += agent.MemoryBytes / (1024 * 1024)
			} else {
				total.MemoryMB += 512 // fallback default if not recorded
			}
			if agent.VCPUs > 0 {
				total.VCPUs += agent.VCPUs
			} else {
				total.VCPUs++ // fallback default: 1 vCPU
			}
		}
	}
	return total
}

// Ensure Backend implements the interface.
var _ backend.Backend = (*Backend)(nil)
