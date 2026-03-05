// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package backend — AgentManager provides a backend-agnostic agent lifecycle
// controller. It routes Create/Start/Stop/Destroy calls to the appropriate
// backend based on the agent's configured runtime backend.
package backend

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"

	"github.com/brmurrell3/hive/internal/types"
)

// AgentManager is the primary lifecycle controller for agents. It replaces
// direct vm.Manager usage by routing operations to the correct backend
// based on each agent's configured runtime.backend field.
type AgentManager struct {
	registry       *Registry
	defaultBackend string
	logger         *slog.Logger
	mu             sync.RWMutex
	agentBackends  map[string]string      // agentID -> backend name
	creating       map[string]struct{}    // agentIDs currently in the Create/Start pipeline (TOCTOU guard)
	forceProcess   bool                   // when true, always use "process" backend regardless of manifest
}

// NewAgentManager creates a new AgentManager with the given backend registry.
// defaultBackend is the name of the backend to use when an agent doesn't
// specify one (typically "firecracker" for backward compatibility).
func NewAgentManager(registry *Registry, defaultBackend string, logger *slog.Logger) *AgentManager {
	return &AgentManager{
		registry:       registry,
		defaultBackend: defaultBackend,
		logger:         logger.With("component", "agent-manager"),
		agentBackends:  make(map[string]string),
		creating:       make(map[string]struct{}),
	}
}

// SetForceProcess enables or disables forced process backend mode.
// When enabled, resolveBackend always returns the "process" backend
// regardless of the agent manifest's runtime.backend field.
func (m *AgentManager) SetForceProcess(force bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.forceProcess = force
}

// resolveBackend determines which backend to use for the given agent manifest.
func (m *AgentManager) resolveBackend(spec *types.AgentManifest) (Backend, error) {
	m.mu.RLock()
	force := m.forceProcess
	m.mu.RUnlock()

	if force {
		m.logger.Warn("force-process-backend active: overriding backend to process",
			"agent_id", spec.Metadata.ID,
			"configured_backend", spec.Spec.Runtime.Backend,
		)
		return m.registry.Get("process")
	}

	backendName := spec.Spec.Runtime.Backend
	if backendName == "" {
		backendName = m.defaultBackend
	}
	return m.registry.Get(backendName)
}

// getBackendForAgent returns the backend assigned to a running agent.
func (m *AgentManager) getBackendForAgent(agentID string) (Backend, error) {
	m.mu.RLock()
	backendName, ok := m.agentBackends[agentID]
	m.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("agent %q not managed by AgentManager", agentID)
	}
	return m.registry.Get(backendName)
}

// StartAgent creates and starts an agent on the appropriate backend.
// This is the high-level entry point that maps to the old vm.Manager.StartAgent.
func (m *AgentManager) StartAgent(ctx context.Context, spec *types.AgentManifest) error {
	agentID := spec.Metadata.ID

	// Acquire write lock to atomically check both agentBackends and creating
	// maps, preventing duplicate Create calls from concurrent StartAgent
	// invocations (BE-H7).
	m.mu.Lock()
	if _, exists := m.agentBackends[agentID]; exists {
		m.mu.Unlock()
		return fmt.Errorf("agent %s is already managed by AgentManager", agentID)
	}
	if _, inProgress := m.creating[agentID]; inProgress {
		m.mu.Unlock()
		return fmt.Errorf("agent %s is already being created", agentID)
	}
	m.creating[agentID] = struct{}{}
	m.mu.Unlock()

	// Ensure the creating sentinel is removed on all exit paths.
	defer func() {
		m.mu.Lock()
		delete(m.creating, agentID)
		m.mu.Unlock()
	}()

	return m.startAgentLocked(ctx, spec)
}

// startAgentLocked performs the actual create+start sequence. The caller must
// have already placed agentID in the `creating` map to prevent concurrent
// StartAgent or RestartAgent calls from racing. This method does NOT manage
// the creating sentinel — that is the caller's responsibility.
func (m *AgentManager) startAgentLocked(ctx context.Context, spec *types.AgentManifest) error {
	agentID := spec.Metadata.ID

	b, err := m.resolveBackend(spec)
	if err != nil {
		return fmt.Errorf("resolving backend for agent %s: %w", agentID, err)
	}

	m.logger.Info("creating agent instance",
		"agent_id", agentID,
		"backend", b.Name(),
	)

	_, err = b.Create(ctx, spec)
	if err != nil {
		return fmt.Errorf("creating agent %s on %s: %w", agentID, b.Name(), err)
	}

	// Track which backend owns this agent.
	m.mu.Lock()
	if _, exists := m.agentBackends[agentID]; exists {
		m.mu.Unlock()
		_ = b.Destroy(ctx, agentID)
		return fmt.Errorf("agent %s was concurrently started", agentID)
	}
	m.agentBackends[agentID] = b.Name()
	m.mu.Unlock()

	if err := b.Start(ctx, agentID); err != nil {
		// Clean up on start failure.
		_ = b.Destroy(ctx, agentID)
		m.mu.Lock()
		delete(m.agentBackends, agentID)
		m.mu.Unlock()
		return fmt.Errorf("starting agent %s on %s: %w", agentID, b.Name(), err)
	}

	m.logger.Info("agent started",
		"agent_id", agentID,
		"backend", b.Name(),
	)
	return nil
}

// StopAgent stops a running agent but keeps it tracked in agentBackends so
// that DestroyAgent can still locate the backend. Callers that want a full
// teardown should call DestroyAgent instead.
func (m *AgentManager) StopAgent(ctx context.Context, agentID string) error {
	b, err := m.getBackendForAgent(agentID)
	if err != nil {
		return err
	}
	return b.Stop(ctx, agentID)
}

// DestroyAgent destroys an agent and releases all resources.
func (m *AgentManager) DestroyAgent(ctx context.Context, agentID string) error {
	b, err := m.getBackendForAgent(agentID)
	if err != nil {
		return err
	}

	if err := b.Destroy(ctx, agentID); err != nil {
		return err
	}

	m.mu.Lock()
	delete(m.agentBackends, agentID)
	m.mu.Unlock()

	return nil
}

// RestartAgent tears down any existing instance for the agent and starts a
// fresh one. It calls Stop/Destroy on the existing backend first, only
// removing the agent from tracking after Destroy succeeds (BE-H3). If
// Destroy fails, the tracking entry is kept so the agent can be retried.
//
// BM-C3: The agent is added to the `creating` map before releasing the lock,
// preventing concurrent StartAgent calls from racing with the in-flight
// restart. The sentinel is removed via defer on all exit paths.
func (m *AgentManager) RestartAgent(ctx context.Context, spec *types.AgentManifest) error {
	agentID := spec.Metadata.ID
	m.logger.Info("restarting agent", "agent_id", agentID)

	// Acquire write lock to atomically check state and place the creating
	// sentinel, preventing concurrent StartAgent from racing (BM-C3).
	m.mu.Lock()
	if _, inProgress := m.creating[agentID]; inProgress {
		m.mu.Unlock()
		return fmt.Errorf("agent %s is already being created", agentID)
	}
	m.creating[agentID] = struct{}{}
	backendName, exists := m.agentBackends[agentID]
	m.mu.Unlock()

	// Ensure the creating sentinel is removed on all exit paths.
	defer func() {
		m.mu.Lock()
		delete(m.creating, agentID)
		m.mu.Unlock()
	}()

	if exists {
		b, err := m.registry.Get(backendName)
		if err != nil {
			return fmt.Errorf("resolving backend %q for agent %s during restart: %w", backendName, agentID, err)
		}

		if stopErr := b.Stop(ctx, agentID); stopErr != nil {
			m.logger.Warn("error stopping agent during restart",
				"agent_id", agentID,
				"error", stopErr,
			)
		}
		if destroyErr := b.Destroy(ctx, agentID); destroyErr != nil {
			m.logger.Warn("error destroying agent during restart, keeping tracking entry",
				"agent_id", agentID,
				"error", destroyErr,
			)
			return fmt.Errorf("destroying agent %s during restart: %w", agentID, destroyErr)
		}

		// Only remove tracking after Destroy succeeds.
		m.mu.Lock()
		delete(m.agentBackends, agentID)
		m.mu.Unlock()
	}

	// Use startAgentLocked since we already hold the creating sentinel.
	return m.startAgentLocked(ctx, spec)
}

// Status returns the status of an agent.
func (m *AgentManager) Status(ctx context.Context, agentID string) (InstanceStatus, error) {
	b, err := m.getBackendForAgent(agentID)
	if err != nil {
		return InstanceStatus{State: "unknown"}, err
	}
	return b.Status(ctx, agentID)
}

// Logs returns a log reader for an agent.
func (m *AgentManager) Logs(ctx context.Context, agentID string, opts LogOpts) (io.ReadCloser, error) {
	b, err := m.getBackendForAgent(agentID)
	if err != nil {
		return nil, err
	}
	return b.Logs(ctx, agentID, opts)
}

// StopVM implements the production.VMAccess interface for graceful shutdown.
func (m *AgentManager) StopVM(ctx context.Context, agentID string) error {
	return m.StopAgent(ctx, agentID)
}

// BackendForAgent returns the name of the backend managing the given agent.
func (m *AgentManager) BackendForAgent(agentID string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.agentBackends[agentID]
}

// RegisterAgent records which backend is managing an existing agent.
// Used during startup reconciliation to re-populate the agent-backend mapping.
func (m *AgentManager) RegisterAgent(agentID, backendName string) {
	m.mu.Lock()
	m.agentBackends[agentID] = backendName
	m.mu.Unlock()
}

// RegisterAgentFromState records which backend is managing an existing agent,
// inferring from the runtime.backend field in the manifest if backendName is empty.
func (m *AgentManager) RegisterAgentFromState(agentID, backendName string) {
	if backendName == "" {
		backendName = m.defaultBackend
	}
	m.mu.Lock()
	m.agentBackends[agentID] = backendName
	m.mu.Unlock()
}
