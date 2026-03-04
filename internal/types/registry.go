// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package types

import (
	"fmt"
	"sort"
	"sync"
)

// CapabilityRegistryEntry describes capabilities registered by a single agent.
type CapabilityRegistryEntry struct {
	TeamID       string            `json:"team_id"`
	Tier         string            `json:"tier"`
	NodeID       string            `json:"node_id,omitempty"`
	Capabilities []AgentCapability `json:"capabilities"`
}

// CapabilityRegistry is the in-memory (and persisted) capability registry.
// All methods are safe for concurrent use.
//
// Locking note: When the registry is embedded inside a Store, the Store's
// RWMutex is the authoritative lock and all access goes through Store methods
// (RegisterCapabilities, DeregisterCapabilities, GetCapabilityRegistry). The
// registry's own mutex exists for standalone usage outside the Store (e.g.,
// in tests or components that hold a registry directly without a Store).
// Callers must never hold both locks simultaneously.
type CapabilityRegistry struct {
	mu     sync.RWMutex
	Agents map[string]*CapabilityRegistryEntry `json:"agents"` // keyed by agent ID
}

// NewCapabilityRegistry creates an empty capability registry.
func NewCapabilityRegistry() *CapabilityRegistry {
	return &CapabilityRegistry{
		Agents: make(map[string]*CapabilityRegistryEntry),
	}
}

// Register adds or updates an agent's capabilities.
// Returns an error if any capability name fails NATS subject component validation.
func (r *CapabilityRegistry) Register(agentID, teamID, tier, nodeID string, caps []AgentCapability) error {
	for _, cap := range caps {
		if err := ValidateSubjectComponent("capability name", cap.Name); err != nil {
			return fmt.Errorf("registering capabilities for agent %q: %w", agentID, err)
		}
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	capsCopy := make([]AgentCapability, len(caps))
	copy(capsCopy, caps)
	for i := range capsCopy {
		if capsCopy[i].Inputs != nil {
			inputs := make([]CapabilityParam, len(capsCopy[i].Inputs))
			copy(inputs, capsCopy[i].Inputs)
			for j := range inputs {
				if inputs[j].Required != nil {
					req := *inputs[j].Required
					inputs[j].Required = &req
				}
			}
			capsCopy[i].Inputs = inputs
		}
		if capsCopy[i].Outputs != nil {
			outputs := make([]CapabilityParam, len(capsCopy[i].Outputs))
			copy(outputs, capsCopy[i].Outputs)
			for j := range outputs {
				if outputs[j].Required != nil {
					req := *outputs[j].Required
					outputs[j].Required = &req
				}
			}
			capsCopy[i].Outputs = outputs
		}
	}
	r.Agents[agentID] = &CapabilityRegistryEntry{
		TeamID:       teamID,
		Tier:         tier,
		NodeID:       nodeID,
		Capabilities: capsCopy,
	}
	return nil
}

// Deregister removes an agent's capabilities.
func (r *CapabilityRegistry) Deregister(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.Agents, agentID)
}

// FindByCapability returns all agents that provide a given capability name.
func (r *CapabilityRegistry) FindByCapability(name string) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var result []string
	for agentID, entry := range r.Agents {
		for _, cap := range entry.Capabilities {
			if cap.Name == name {
				result = append(result, agentID)
				break
			}
		}
	}
	sort.Strings(result)
	return result
}

// AllCapabilities returns a flat list of all capabilities with their agent IDs.
func (r *CapabilityRegistry) AllCapabilities() map[string][]string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make(map[string][]string)
	for agentID, entry := range r.Agents {
		for _, cap := range entry.Capabilities {
			result[cap.Name] = append(result[cap.Name], agentID)
		}
	}
	return result
}
