package config

import (
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/brmurrell3/hive/internal/types"
)

// ValidationError collects multiple validation errors.
type ValidationError struct {
	Errors []string
}

func (v *ValidationError) Error() string {
	return fmt.Sprintf("validation failed with %d error(s):\n  - %s", len(v.Errors), strings.Join(v.Errors, "\n  - "))
}

func (v *ValidationError) add(msg string) {
	v.Errors = append(v.Errors, msg)
}

func (v *ValidationError) addf(format string, args ...interface{}) {
	v.Errors = append(v.Errors, fmt.Sprintf(format, args...))
}

func (v *ValidationError) hasErrors() bool {
	return len(v.Errors) > 0
}

func (v *ValidationError) errorOrNil() error {
	if v.hasErrors() {
		return v
	}
	return nil
}

var agentIDRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
var secretRefRegex = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)

// reservedProviderNames lists provider names that cannot be used as model registry names.
var reservedProviderNames = map[string]bool{
	"anthropic": true,
	"openai":    true,
	"ollama":    true,
	"google":    true,
	"mistral":   true,
	"cohere":    true,
}

func validateCluster(cfg *types.ClusterConfig) error {
	ve := &ValidationError{}

	if cfg.APIVersion != "hive/v1" {
		ve.addf("apiVersion must be \"hive/v1\", got %q", cfg.APIVersion)
	}
	if cfg.Kind != "Cluster" {
		ve.addf("kind must be \"Cluster\", got %q", cfg.Kind)
	}
	if cfg.Metadata.Name == "" {
		ve.add("metadata.name is required")
	}

	// Validate NATS port (0 and -1 are allowed as they mean random/auto)
	if cfg.Spec.NATS.Port < -1 || cfg.Spec.NATS.Port > 65535 {
		ve.addf("spec.nats.port must be between -1 and 65535, got %d", cfg.Spec.NATS.Port)
	}
	if cfg.Spec.NATS.ClusterPort < -1 || cfg.Spec.NATS.ClusterPort > 65535 {
		ve.addf("spec.nats.clusterPort must be between -1 and 65535, got %d", cfg.Spec.NATS.ClusterPort)
	}

	// Validate restart policy
	validPolicies := map[string]bool{"always": true, "on-failure": true, "never": true}
	if !validPolicies[cfg.Spec.Defaults.Restart.Policy] {
		ve.addf("spec.defaults.restart.policy must be one of [always, on-failure, never], got %q", cfg.Spec.Defaults.Restart.Policy)
	}

	return ve.errorOrNil()
}

// ValidateDesiredState validates the complete desired state including cross-references.
func ValidateDesiredState(ds *types.DesiredState) error {
	ve := &ValidationError{}

	// Build lookup maps
	agentsByID := ds.Agents
	teamsByID := ds.Teams

	// Track seen agent IDs for duplicate detection (already handled by map key,
	// but LoadAgents might silently overwrite - we detect at directory level)
	// The loader puts them in a map keyed by ID, so duplicates across dirs
	// would need to be caught at load time. For now, validate in-memory state.

	// Validate each agent
	for id, agent := range agentsByID {
		validateAgent(ve, id, agent, teamsByID, ds.Cluster)
	}

	// Validate each team
	for id, team := range teamsByID {
		validateTeam(ve, id, team, agentsByID)
	}

	// Rule 11: Director agentId must reference existing agent with no team.
	// Rule 12: Director agent must have tier vm or native.
	if ds.Cluster != nil && ds.Cluster.Spec.Director.AgentID != "" {
		dirAgentID := ds.Cluster.Spec.Director.AgentID
		dirAgent, ok := agentsByID[dirAgentID]
		if !ok {
			ve.addf("spec.director.agentId %q references nonexistent agent", dirAgentID)
		} else {
			if dirAgent.Metadata.Team != "" {
				ve.addf("spec.director.agentId %q must not have metadata.team set (got %q)", dirAgentID, dirAgent.Metadata.Team)
			}
			if dirAgent.Spec.Tier != "" && dirAgent.Spec.Tier != "vm" && dirAgent.Spec.Tier != "native" {
				ve.addf("spec.director.agentId %q must have tier vm or native, got %q", dirAgentID, dirAgent.Spec.Tier)
			}
		}
	}

	// Rule 13: Model provider names must not shadow reserved provider names.
	if ds.Cluster != nil {
		for _, model := range ds.Cluster.Spec.Models {
			if reservedProviderNames[model.Name] {
				ve.addf("spec.models entry name %q shadows reserved provider name", model.Name)
			}
		}
	}

	// Rule 17: User IDs must be unique.
	// Rule 18: User team references must be valid team IDs or "all".
	// Rule 19: User agent references must be valid agent IDs.
	if ds.Cluster != nil {
		seenUserIDs := make(map[string]bool)
		for _, user := range ds.Cluster.Spec.Users {
			if user.ID == "" {
				ve.add("spec.users: user id is required")
				continue
			}
			if seenUserIDs[user.ID] {
				ve.addf("spec.users: duplicate user id %q", user.ID)
			}
			seenUserIDs[user.ID] = true

			// Validate user role is one of the allowed values.
			validRoles := map[string]bool{"admin": true, "operator": true, "viewer": true}
			if user.Role == "" {
				ve.addf("spec.users: user %q role is required", user.ID)
			} else if !validRoles[user.Role] {
				ve.addf("spec.users: user %q role must be one of [admin, operator, viewer], got %q", user.ID, user.Role)
			}

			// Rule 18: Validate team references.
			for _, teamRef := range user.Teams {
				if teamRef == "all" {
					continue
				}
				if _, ok := teamsByID[teamRef]; !ok {
					ve.addf("spec.users: user %q references nonexistent team %q", user.ID, teamRef)
				}
			}

			// Rule 19: Validate agent references.
			for _, agentRef := range user.Agents {
				if _, ok := agentsByID[agentRef]; !ok {
					ve.addf("spec.users: user %q references nonexistent agent %q", user.ID, agentRef)
				}
			}
		}
	}

	return ve.errorOrNil()
}

func validateAgent(ve *ValidationError, id string, agent *types.AgentManifest, teams map[string]*types.TeamManifest, cluster *types.ClusterConfig) {
	prefix := fmt.Sprintf("agent %q", id)

	// Validate apiVersion and kind
	if agent.APIVersion != "hive/v1" {
		ve.addf("%s: apiVersion must be \"hive/v1\", got %q", prefix, agent.APIVersion)
	}
	if agent.Kind != "Agent" {
		ve.addf("%s: kind must be \"Agent\", got %q", prefix, agent.Kind)
	}

	// Validate metadata.id
	if agent.Metadata.ID == "" {
		ve.addf("%s: metadata.id is required", prefix)
	} else if !agentIDRegex.MatchString(agent.Metadata.ID) {
		ve.addf("%s: metadata.id %q does not match pattern [a-z0-9][a-z0-9-]{0,62}", prefix, agent.Metadata.ID)
	}

	// Validate metadata.team references existing team
	if agent.Metadata.Team != "" {
		if _, ok := teams[agent.Metadata.Team]; !ok {
			ve.addf("%s: metadata.team %q references nonexistent team", prefix, agent.Metadata.Team)
		}
	}

	// Validate runtime.type is required
	validRuntimeTypes := map[string]bool{
		"openclaw": true, "custom": true,
		"firmware-c": true, "firmware-micropython": true,
	}
	if agent.Spec.Runtime.Type == "" {
		ve.addf("%s: spec.runtime.type is required", prefix)
	} else if !validRuntimeTypes[agent.Spec.Runtime.Type] {
		ve.addf("%s: spec.runtime.type must be one of [openclaw, custom, firmware-c, firmware-micropython], got %q", prefix, agent.Spec.Runtime.Type)
	}

	// Validate tier
	if agent.Spec.Tier != "" {
		validTiers := map[string]bool{"vm": true, "native": true, "firmware": true}
		if !validTiers[agent.Spec.Tier] {
			ve.addf("%s: spec.tier must be one of [vm, native, firmware], got %q", prefix, agent.Spec.Tier)
		}
	}

	// Validate tier/runtime compatibility (rule 8)
	if agent.Spec.Tier != "" && agent.Spec.Runtime.Type != "" {
		switch agent.Spec.Tier {
		case "vm", "native":
			if agent.Spec.Runtime.Type != "openclaw" && agent.Spec.Runtime.Type != "custom" {
				ve.addf("%s: tier %q is not compatible with runtime type %q", prefix, agent.Spec.Tier, agent.Spec.Runtime.Type)
			}
		case "firmware":
			if agent.Spec.Runtime.Type != "firmware-c" && agent.Spec.Runtime.Type != "firmware-micropython" {
				ve.addf("%s: tier %q is not compatible with runtime type %q", prefix, agent.Spec.Tier, agent.Spec.Runtime.Type)
			}
		}
	}

	// Validate resources
	if agent.Spec.Resources.Memory != "" {
		if _, err := ParseMemory(agent.Spec.Resources.Memory); err != nil {
			ve.addf("%s: spec.resources.memory %q is invalid: %v", prefix, agent.Spec.Resources.Memory, err)
		}
	}
	if agent.Spec.Resources.VCPUs < 0 {
		ve.addf("%s: spec.resources.vcpus must be positive, got %d", prefix, agent.Spec.Resources.VCPUs)
	}

	// Validate capabilities (rule 7: unique names within agent)
	validParamTypes := map[string]bool{"string": true, "int": true, "float": true, "bool": true, "bytes": true}
	capNames := make(map[string]bool)
	for _, cap := range agent.Spec.Capabilities {
		if cap.Name == "" {
			ve.addf("%s: capability name is required", prefix)
			continue
		}
		if cap.Description == "" {
			ve.addf("%s: capability %q description is required", prefix, cap.Name)
		}
		if capNames[cap.Name] {
			ve.addf("%s: duplicate capability name %q", prefix, cap.Name)
		}
		capNames[cap.Name] = true
		for _, param := range cap.Inputs {
			if param.Type != "" && !validParamTypes[param.Type] {
				ve.addf("%s: capability %q input %q type must be one of [string, int, float, bool, bytes], got %q", prefix, cap.Name, param.Name, param.Type)
			}
		}
		for _, param := range cap.Outputs {
			if param.Type != "" && !validParamTypes[param.Type] {
				ve.addf("%s: capability %q output %q type must be one of [string, int, float, bool, bytes], got %q", prefix, cap.Name, param.Name, param.Type)
			}
		}
	}

	// Validate volumes reference team shared_volumes (rule 2, 9)
	if len(agent.Spec.Volumes) > 0 {
		if agent.Spec.Tier != "" && agent.Spec.Tier != "vm" {
			ve.addf("%s: volumes are only valid for vm tier", prefix)
		}
		if agent.Metadata.Team != "" {
			if team, ok := teams[agent.Metadata.Team]; ok {
				svNames := make(map[string]bool)
				for _, sv := range team.Spec.SharedVolumes {
					svNames[sv.Name] = true
				}
				validVolumeAccess := map[string]bool{"read-only": true, "read-write": true}
				for _, vol := range agent.Spec.Volumes {
					if !svNames[vol.Name] {
						ve.addf("%s: volume %q references nonexistent shared_volume in team %q", prefix, vol.Name, agent.Metadata.Team)
					}
					if vol.Access != "" && !validVolumeAccess[vol.Access] {
						ve.addf("%s: volume %q access must be one of [read-only, read-write], got %q", prefix, vol.Name, vol.Access)
					}
				}
			}
		}
	}

	// Validate network egress only for vm tier (rule 10)
	if agent.Spec.Network.Egress != "" && agent.Spec.Tier != "" && agent.Spec.Tier != "vm" {
		ve.addf("%s: network egress is only valid for vm tier", prefix)
	}
	if agent.Spec.Network.Egress != "" {
		validEgress := map[string]bool{"none": true, "restricted": true, "full": true}
		if !validEgress[agent.Spec.Network.Egress] {
			ve.addf("%s: spec.network.egress must be one of [none, restricted, full], got %q", prefix, agent.Spec.Network.Egress)
		}
	}

	// Validate mode only for firmware tier (rule 15)
	if agent.Spec.Mode != "" && agent.Spec.Tier != "" && agent.Spec.Tier != "firmware" {
		ve.addf("%s: mode field is only valid for firmware tier", prefix)
	}

	// Validate firmware fields required for firmware tier (rule 14)
	if agent.Spec.Tier == "firmware" {
		if agent.Spec.Firmware.Platform == "" {
			ve.addf("%s: spec.firmware.platform is required for firmware tier", prefix)
		}
		if agent.Spec.Firmware.Board == "" {
			ve.addf("%s: spec.firmware.board is required for firmware tier", prefix)
		}
	}

	// Validate restart policy
	if agent.Spec.Restart.Policy != "" {
		validPolicies := map[string]bool{"always": true, "on-failure": true, "never": true}
		if !validPolicies[agent.Spec.Restart.Policy] {
			ve.addf("%s: spec.restart.policy must be one of [always, on-failure, never], got %q", prefix, agent.Spec.Restart.Policy)
		}
	}

	// Rule 6: Validate secret references (${SECRET_NAME}) in runtime.model.env
	// resolve against spec.secrets in cluster config.
	if cluster != nil && len(agent.Spec.Runtime.Model.Env) > 0 {
		secrets := cluster.Spec.Secrets
		for envKey, envVal := range agent.Spec.Runtime.Model.Env {
			matches := secretRefRegex.FindAllStringSubmatch(envVal, -1)
			for _, match := range matches {
				secretName := match[1]
				if secrets == nil {
					ve.addf("%s: env %q references secret ${%s} but spec.secrets is not defined", prefix, envKey, secretName)
				} else if _, ok := secrets[secretName]; !ok {
					ve.addf("%s: env %q references secret ${%s} which is not in spec.secrets", prefix, envKey, secretName)
				}
			}
		}
	}

	// Rule 16: Placement nodeId warning if set (node may not be registered at
	// parse time, so this is a warning, not an error).
	if agent.Spec.Placement.NodeID != "" {
		slog.Warn("agent placement.nodeId is set; node registration cannot be verified at parse time",
			"agent_id", id, "node_id", agent.Spec.Placement.NodeID)
	}
}

func validateTeam(ve *ValidationError, id string, team *types.TeamManifest, agents map[string]*types.AgentManifest) {
	prefix := fmt.Sprintf("team %q", id)

	if team.APIVersion != "hive/v1" {
		ve.addf("%s: apiVersion must be \"hive/v1\", got %q", prefix, team.APIVersion)
	}
	if team.Kind != "Team" {
		ve.addf("%s: kind must be \"Team\", got %q", prefix, team.Kind)
	}

	if team.Metadata.ID == "" {
		ve.addf("%s: metadata.id is required", prefix)
	} else if !agentIDRegex.MatchString(team.Metadata.ID) {
		ve.addf("%s: metadata.id %q does not match pattern [a-z0-9][a-z0-9-]{0,62}", prefix, team.Metadata.ID)
	}

	// Validate lead (rule 3)
	if team.Spec.Lead != "" {
		leadAgent, ok := agents[team.Spec.Lead]
		if !ok {
			ve.addf("%s: lead %q references nonexistent agent", prefix, team.Spec.Lead)
		} else if leadAgent.Metadata.Team != id {
			ve.addf("%s: lead %q has metadata.team %q, expected %q", prefix, team.Spec.Lead, leadAgent.Metadata.Team, id)
		}
	}

	// Validate shared_volumes access values
	validSharedVolumeAccess := map[string]bool{"read-only": true, "read-write": true}
	for _, sv := range team.Spec.SharedVolumes {
		if sv.Access != "" && !validSharedVolumeAccess[sv.Access] {
			ve.addf("%s: shared_volume %q access must be one of [read-only, read-write], got %q", prefix, sv.Name, sv.Access)
		}
	}
}

// ParseMemory parses a memory string like "512Mi", "1Gi", "256MB" into bytes.
func ParseMemory(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty memory string")
	}

	// Try to find where the numeric part ends
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("invalid memory format %q: no numeric value", s)
	}

	numStr := s[:i]
	suffix := s[i:]

	var num float64
	if _, err := fmt.Sscanf(numStr, "%f", &num); err != nil {
		return 0, fmt.Errorf("invalid memory format %q: %w", s, err)
	}

	multipliers := map[string]float64{
		"":   1,
		"B":  1,
		"K":  1024,
		"Ki": 1024,
		"KB": 1000,
		"M":  1024 * 1024,
		"Mi": 1024 * 1024,
		"MB": 1000 * 1000,
		"G":  1024 * 1024 * 1024,
		"Gi": 1024 * 1024 * 1024,
		"GB": 1000 * 1000 * 1000,
	}

	mult, ok := multipliers[suffix]
	if !ok {
		return 0, fmt.Errorf("invalid memory suffix %q in %q", suffix, s)
	}

	return int64(num * mult), nil
}
