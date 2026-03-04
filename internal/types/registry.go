package types

// CapabilityRegistryEntry describes capabilities registered by a single agent.
type CapabilityRegistryEntry struct {
	TeamID       string            `json:"team_id"`
	Tier         string            `json:"tier"`
	NodeID       string            `json:"node_id,omitempty"`
	Capabilities []AgentCapability `json:"capabilities"`
}

// CapabilityRegistry is the in-memory (and persisted) capability registry.
type CapabilityRegistry struct {
	Agents map[string]*CapabilityRegistryEntry `json:"agents"` // keyed by agent ID
}

// NewCapabilityRegistry creates an empty capability registry.
func NewCapabilityRegistry() *CapabilityRegistry {
	return &CapabilityRegistry{
		Agents: make(map[string]*CapabilityRegistryEntry),
	}
}

// Register adds or updates an agent's capabilities.
func (r *CapabilityRegistry) Register(agentID, teamID, tier, nodeID string, caps []AgentCapability) {
	r.Agents[agentID] = &CapabilityRegistryEntry{
		TeamID:       teamID,
		Tier:         tier,
		NodeID:       nodeID,
		Capabilities: caps,
	}
}

// Deregister removes an agent's capabilities.
func (r *CapabilityRegistry) Deregister(agentID string) {
	delete(r.Agents, agentID)
}

// FindByCapability returns all agents that provide a given capability name.
func (r *CapabilityRegistry) FindByCapability(name string) []string {
	var result []string
	for agentID, entry := range r.Agents {
		for _, cap := range entry.Capabilities {
			if cap.Name == name {
				result = append(result, agentID)
				break
			}
		}
	}
	return result
}

// AllCapabilities returns a flat list of all capabilities with their agent IDs.
func (r *CapabilityRegistry) AllCapabilities() map[string][]string {
	result := make(map[string][]string)
	for agentID, entry := range r.Agents {
		for _, cap := range entry.Capabilities {
			result[cap.Name] = append(result[cap.Name], agentID)
		}
	}
	return result
}
