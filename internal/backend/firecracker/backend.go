// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package firecracker implements the Backend interface using Firecracker VMs.
// It wraps the existing vm.Manager to provide the higher-level Backend
// abstraction while preserving all existing Firecracker functionality.
package firecracker

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"os"
	goruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"

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

// Create provisions and starts a Firecracker VM for the given agent.
//
// Design constraint (BE-H4): the underlying vm.Manager.StartAgent performs
// both VM creation and boot in a single atomic operation — it allocates a
// CID, creates the root filesystem, launches the Firecracker process, and
// waits for the guest kernel to boot. Splitting these into separate
// Create + Start steps is not feasible without a major refactor of the
// vm.Manager, because the Firecracker API socket is only available after
// the process is launched, and resource accounting (memory, CID) must be
// committed atomically with VM creation to prevent leaks.
//
// Consequently, Create calls vmMgr.StartAgent (which both creates and
// starts the VM), and Start below is intentionally a no-op. This is safe
// because the Backend interface contract allows Create to leave the
// instance in a runnable state, and callers always invoke Create before
// Start.
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

// Start is intentionally a no-op for the Firecracker backend.
//
// The VM is already running after Create because vm.Manager.StartAgent
// performs both provisioning and boot atomically. See the Create comment
// above for the full rationale (BE-H4).
func (b *Backend) Start(ctx context.Context, id string) error {
	return nil
}

func (b *Backend) Stop(ctx context.Context, id string) error {
	return b.vmMgr.StopAgent(ctx, id)
}

func (b *Backend) Destroy(ctx context.Context, id string) error {
	err := b.vmMgr.DestroyAgent(ctx, id)
	if err != nil {
		// Keep the instance in the map so the caller can retry.
		return err
	}

	b.mu.Lock()
	delete(b.instances, id)
	b.mu.Unlock()

	return nil
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

// systemMemoryMB returns the total physical system RAM in megabytes.
// It uses platform-specific methods:
//   - Linux:  parses /proc/meminfo for MemTotal
//   - macOS:  reads hw.memsize via syscall.Sysctl
//
// Returns 0 if detection fails so the caller can apply a fallback.
func systemMemoryMB() int64 {
	switch goruntime.GOOS {
	case "linux":
		return linuxMemoryMB()
	case "darwin":
		return darwinMemoryMB()
	default:
		return 0
	}
}

// linuxMemoryMB parses /proc/meminfo to extract MemTotal in MB.
func linuxMemoryMB() int64 {
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

// darwinMemoryMB reads hw.memsize via syscall.Sysctl to get total RAM in MB.
func darwinMemoryMB() int64 {
	val, err := syscall.Sysctl("hw.memsize")
	if err != nil || len(val) == 0 {
		return 0
	}
	// syscall.Sysctl returns a raw byte string; hw.memsize is a uint64 in
	// host byte order (little-endian on all supported Apple hardware).
	b := []byte(val)
	// The kernel may or may not include a trailing NUL byte.
	// Ensure we have at least 8 bytes for the uint64.
	if len(b) < 8 {
		return 0
	}
	memBytes := binary.LittleEndian.Uint64(b[:8])
	return int64(memBytes / (1024 * 1024))
}

func (b *Backend) Available() backend.Resources {
	cpuCount := goruntime.NumCPU()

	// Detect actual system RAM using platform-specific methods.
	memTotalMB := systemMemoryMB()
	if memTotalMB < 256 {
		memTotalMB = 8192 // fallback if detection fails or returns unreasonably low value
	}

	return backend.Resources{
		MemoryMB: memTotalMB,
		VCPUs:    cpuCount,
		DiskMB:   10240,
	}
}

func (b *Backend) Allocated() backend.Resources {
	b.mu.RLock()
	tracked := make(map[string]struct{}, len(b.instances))
	for id := range b.instances {
		tracked[id] = struct{}{}
	}
	b.mu.RUnlock()

	var total backend.Resources
	for _, agent := range b.store.AllAgents() {
		// Only count agents actually managed by this backend.
		if _, ok := tracked[agent.ID]; !ok {
			continue
		}
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
