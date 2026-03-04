// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package backend defines the Backend interface for pluggable agent execution.
// Implementations include Firecracker VMs, local processes, and systemd-nspawn
// containers. The Backend interface subsumes the Hypervisor's responsibilities
// at a higher abstraction level.
package backend

import (
	"context"
	"io"

	"github.com/brmurrell3/hive/internal/types"
)

// Backend is the interface for agent execution backends.
// Implementations manage the full lifecycle of agent instances: creation,
// start, stop, destroy, and resource tracking.
type Backend interface {
	// Name returns the backend identifier (e.g., "firecracker", "process", "nspawn").
	Name() string

	// Capabilities returns what this backend supports.
	Capabilities() BackendCaps

	// Create provisions a new agent instance from the given spec.
	// The instance is created but not started.
	Create(ctx context.Context, spec *types.AgentManifest) (Instance, error)

	// Start begins execution of a previously created instance.
	Start(ctx context.Context, id string) error

	// Stop gracefully stops a running instance.
	Stop(ctx context.Context, id string) error

	// Destroy forcefully terminates an instance and cleans up all resources.
	Destroy(ctx context.Context, id string) error

	// Status returns the current status of an instance.
	Status(ctx context.Context, id string) (InstanceStatus, error)

	// Logs returns a reader for the instance's log output.
	Logs(ctx context.Context, id string, opts LogOpts) (io.ReadCloser, error)

	// Available returns the total resources available to this backend.
	Available() Resources

	// Allocated returns the resources currently in use.
	Allocated() Resources
}

// Instance represents a created agent instance within a backend.
type Instance interface {
	// ID returns the unique instance identifier.
	ID() string

	// AgentID returns the agent ID this instance belongs to.
	AgentID() string

	// Backend returns the name of the backend managing this instance.
	Backend() string
}

// BackendCaps describes what a backend supports.
type BackendCaps struct {
	// Isolation indicates the level of isolation provided.
	// Values: "vm", "container", "process"
	Isolation string

	// SupportsSnapshots indicates whether the backend supports VM snapshots.
	SupportsSnapshots bool

	// SupportsNetworkPolicy indicates whether egress/ingress rules can be enforced.
	SupportsNetworkPolicy bool

	// SupportsMounts indicates whether host filesystem mounts are supported.
	SupportsMounts bool

	// SupportsResourceLimits indicates whether CPU/memory limits are enforced.
	SupportsResourceLimits bool
}

// InstanceStatus describes the current state of an instance.
type InstanceStatus struct {
	State     string    // "pending", "running", "stopped", "failed"
	ExitCode  int       // exit code if stopped/failed
	Resources Resources // current resource usage
}

// Resources describes compute resources.
type Resources struct {
	MemoryMB int64 // memory in megabytes
	VCPUs    int   // virtual CPU count
	DiskMB   int64 // disk in megabytes
}

// LogOpts configures log retrieval.
type LogOpts struct {
	Follow bool // stream new log lines
	Tail   int  // number of recent lines (0 = all)
}
