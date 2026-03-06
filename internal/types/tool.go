package types

// ToolDefinition represents a tool exposed by an agent capability.
// Used by both the capability tool generator and the director agent.
type ToolDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	AgentID     string      `json:"agent_id,omitempty"`
	Inputs      []ToolParam `json:"inputs,omitempty"`
	Outputs     []ToolParam `json:"outputs,omitempty"`
}

// ToolParam describes a single input or output parameter for a tool.
type ToolParam struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}
