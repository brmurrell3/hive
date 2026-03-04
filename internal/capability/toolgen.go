package capability

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/hivehq/hive/internal/types"
)

// ToolDefinition represents an auto-generated tool file for a lead agent.
// Each tool corresponds to a single capability exposed by a worker agent
// within the same team.
type ToolDefinition struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	AgentID     string      `json:"agent_id"`
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

// GenerateTools produces ToolDefinition entries for all agents in a team,
// excluding the lead agent. Each capability declared by an agent becomes
// a separate tool that the lead can invoke.
//
// agents is a map of agentID -> AgentManifest for every agent in the team.
// teamID is the team identifier (used only for descriptive metadata).
func GenerateTools(agents map[string]*types.AgentManifest, teamID string) []ToolDefinition {
	var tools []ToolDefinition

	// Identify the lead agent so we can exclude it.
	leadID := ""
	for _, agent := range agents {
		if agent.Spec.Mode == "lead" {
			leadID = agent.Metadata.ID
			break
		}
	}

	// Sort agent IDs for deterministic output.
	agentIDs := make([]string, 0, len(agents))
	for id := range agents {
		agentIDs = append(agentIDs, id)
	}
	sort.Strings(agentIDs)

	for _, agentID := range agentIDs {
		agent := agents[agentID]

		// Skip the lead agent; it consumes tools, it does not expose them.
		if agentID == leadID {
			continue
		}

		for _, cap := range agent.Spec.Capabilities {
			tool := ToolDefinition{
				Name:        fmt.Sprintf("%s-%s", agentID, cap.Name),
				Description: cap.Description,
				AgentID:     agentID,
			}

			for _, in := range cap.Inputs {
				tool.Inputs = append(tool.Inputs, ToolParam{
					Name:        in.Name,
					Type:        in.Type,
					Description: in.Description,
					Required:    in.IsRequired(),
				})
			}

			for _, out := range cap.Outputs {
				tool.Outputs = append(tool.Outputs, ToolParam{
					Name:        out.Name,
					Type:        out.Type,
					Description: out.Description,
					Required:    out.IsRequired(),
				})
			}

			tools = append(tools, tool)
		}
	}

	return tools
}

// WriteToolFiles writes each ToolDefinition as a JSON file in the given
// directory. Each file is named {agent_id}-{capability_name}.json.
// The directory is created if it does not exist.
func WriteToolFiles(tools []ToolDefinition, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating tool directory %s: %w", dir, err)
	}

	for _, tool := range tools {
		data, err := json.MarshalIndent(tool, "", "  ")
		if err != nil {
			return fmt.Errorf("marshaling tool %s: %w", tool.Name, err)
		}

		filename := filepath.Join(dir, tool.Name+".json")
		if err := os.WriteFile(filename, data, 0o644); err != nil {
			return fmt.Errorf("writing tool file %s: %w", filename, err)
		}
	}

	return nil
}
