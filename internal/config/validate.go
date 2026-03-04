// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package config

import (
	"fmt"
	"log/slog"
	"math"
	"net"
	"os"
	"regexp"
	"strings"

	"github.com/brmurrell3/hive/internal/types"
)

const (
	apiVersionV1     = "hive/v1"
	restartOnFailure = "on-failure"
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

var hiveIDRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
var secretRefRegex = regexp.MustCompile(`\$\{([A-Z_][A-Z0-9_]*)\}`)

// dangerousGuestPaths are system paths that must not be used as mount guest paths.
var dangerousGuestPaths = map[string]bool{
	"/":     true,
	"/etc":  true,
	"/usr":  true,
	"/bin":  true,
	"/sbin": true,
	"/lib":  true,
	"/dev":  true,
	"/proc": true,
	"/sys":  true,
	"/tmp":  true,
	"/var":  true,
	"/boot": true,
	"/root": true,
	"/home": true,
}

// ingressPathInvalidChars matches characters not allowed in ingress paths.
var ingressPathInvalidChars = regexp.MustCompile(`[^a-zA-Z0-9/_\-.]`)

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

	if cfg.APIVersion != apiVersionV1 {
		ve.addf("apiVersion must be \"hive/v1\", got %q", cfg.APIVersion)
	}
	if cfg.Kind != "Cluster" {
		ve.addf("kind must be \"Cluster\", got %q", cfg.Kind)
	}
	if cfg.Metadata.Name == "" {
		ve.add("metadata.name is required")
	} else if !hiveIDRegex.MatchString(cfg.Metadata.Name) {
		ve.addf("metadata.name %q does not match pattern [a-z0-9][a-z0-9-]{0,62}", cfg.Metadata.Name)
	}

	// Validate NATS port (0 and -1 are allowed as they mean random/auto)
	if cfg.Spec.NATS.Port < -1 || cfg.Spec.NATS.Port > 65535 {
		ve.addf("spec.nats.port must be between -1 and 65535, got %d", cfg.Spec.NATS.Port)
	}
	if cfg.Spec.NATS.ClusterPort < -1 || cfg.Spec.NATS.ClusterPort > 65535 {
		ve.addf("spec.nats.clusterPort must be between -1 and 65535, got %d", cfg.Spec.NATS.ClusterPort)
	}

	// Validate NATS mode
	if cfg.Spec.NATS.Mode != "" {
		validModes := map[string]bool{"embedded": true, "external": true}
		if !validModes[cfg.Spec.NATS.Mode] {
			ve.addf("spec.nats.mode must be one of [embedded, external], got %q", cfg.Spec.NATS.Mode)
		}
	}

	// Warn if authToken is empty when using external NATS (where it should be configured).
	// NOTE: Uses global slog logger rather than an injected logger. Refactoring
	// to accept a logger parameter would change many call signatures throughout
	// the validation chain and is deferred as a larger change.
	if cfg.Spec.NATS.Mode == "external" && cfg.Spec.NATS.AuthToken == "" {
		slog.Warn("spec.nats.authToken is empty; external NATS mode typically requires an auth token")
	}

	// Validate restart policy
	validPolicies := map[string]bool{"always": true, restartOnFailure: true, "never": true}
	if !validPolicies[cfg.Spec.Defaults.Restart.Policy] {
		ve.addf("spec.defaults.restart.policy must be one of [always, on-failure, never], got %q", cfg.Spec.Defaults.Restart.Policy)
	}

	// Validate MaxRestarts is non-negative
	if cfg.Spec.Defaults.Restart.MaxRestarts < 0 {
		ve.addf("spec.defaults.restart.maxRestarts must be >= 0, got %d", cfg.Spec.Defaults.Restart.MaxRestarts)
	}

	// Validate health MaxFailures is non-negative
	if cfg.Spec.Defaults.Health.MaxFailures < 0 {
		ve.addf("spec.defaults.health.maxFailures must be >= 0, got %d", cfg.Spec.Defaults.Health.MaxFailures)
	}

	// Warn if health timeout >= interval (timeout should be less than interval
	// so that a health check completes before the next one fires).
	if cfg.Spec.Defaults.Health.Timeout > 0 && cfg.Spec.Defaults.Health.Interval > 0 &&
		cfg.Spec.Defaults.Health.Timeout >= cfg.Spec.Defaults.Health.Interval {
		slog.Warn("spec.defaults.health.timeout >= health.interval; timeout should be less than interval",
			"timeout", cfg.Spec.Defaults.Health.Timeout, "interval", cfg.Spec.Defaults.Health.Interval)
	}

	// Validate dashboard address format if enabled
	if cfg.Spec.Dashboard.Enabled && cfg.Spec.Dashboard.Addr != "" {
		if _, _, err := net.SplitHostPort(cfg.Spec.Dashboard.Addr); err != nil {
			ve.addf("spec.dashboard.addr %q is not a valid host:port address: %v", cfg.Spec.Dashboard.Addr, err)
		}
	}

	// Validate metrics address format if enabled
	if cfg.Spec.Metrics.Enabled && cfg.Spec.Metrics.Addr != "" {
		if _, _, err := net.SplitHostPort(cfg.Spec.Metrics.Addr); err != nil {
			ve.addf("spec.metrics.addr %q is not a valid host:port address: %v", cfg.Spec.Metrics.Addr, err)
		}
	}

	// Warn if any secret value is empty
	for name, value := range cfg.Spec.Secrets {
		if value == "" {
			slog.Warn("spec.secrets: secret has empty value", "name", name)
		}
	}

	// Validate logging retention days if set
	if cfg.Spec.Logging.Enabled && cfg.Spec.Logging.RetentionDays != 0 {
		if cfg.Spec.Logging.RetentionDays < 0 {
			ve.addf("spec.logging.retentionDays must be > 0, got %d", cfg.Spec.Logging.RetentionDays)
		}
	}

	// Validate JetStream store path (path traversal check).
	if cfg.Spec.NATS.JetStream.StorePath != "" {
		if strings.Contains(cfg.Spec.NATS.JetStream.StorePath, "..") {
			ve.add("spec.nats.jetstream.storePath contains path traversal")
		}
	}

	// Validate JetStream MaxMemory and MaxStorage formats.
	if cfg.Spec.NATS.JetStream.MaxMemory != "" {
		if _, err := ParseMemory(cfg.Spec.NATS.JetStream.MaxMemory); err != nil {
			ve.addf("spec.nats.jetstream.maxMemory %q is invalid: %v", cfg.Spec.NATS.JetStream.MaxMemory, err)
		}
	}
	if cfg.Spec.NATS.JetStream.MaxStorage != "" {
		if _, err := ParseDiskSize(cfg.Spec.NATS.JetStream.MaxStorage); err != nil {
			ve.addf("spec.nats.jetstream.maxStorage %q is invalid: %v", cfg.Spec.NATS.JetStream.MaxStorage, err)
		}
	}

	// Validate NATS MaxConnections and MaxSubscriptions.
	if cfg.Spec.NATS.MaxConnections < 0 {
		ve.addf("spec.nats.maxConnections must be >= 0, got %d", cfg.Spec.NATS.MaxConnections)
	}
	if cfg.Spec.NATS.MaxSubscriptions < 0 {
		ve.addf("spec.nats.maxSubscriptions must be >= 0, got %d", cfg.Spec.NATS.MaxSubscriptions)
	}

	// Validate VM paths for path traversal.
	if cfg.Spec.VM.KernelPath != "" && strings.Contains(cfg.Spec.VM.KernelPath, "..") {
		ve.add("spec.vm.kernelPath contains path traversal")
	}
	if cfg.Spec.VM.RootfsPath != "" && strings.Contains(cfg.Spec.VM.RootfsPath, "..") {
		ve.add("spec.vm.rootfsPath contains path traversal")
	}

	// Validate model configs.
	for i, model := range cfg.Spec.Models {
		if model.Name == "" {
			ve.addf("spec.models[%d]: name is required", i)
		}
	}

	// Validate TLS configurations.
	validateTLSConfig(ve, "spec.nats.tls", &cfg.Spec.NATS.TLS)
	if cfg.Spec.Dashboard.Enabled {
		validateTLSConfig(ve, "spec.dashboard.tls", &cfg.Spec.Dashboard.TLS)
	}

	return ve.errorOrNil()
}

// validateTLSConfig validates a TLS configuration block. If TLS is enabled,
// certFile and keyFile must be non-empty and the files must exist on disk.
func validateTLSConfig(ve *ValidationError, prefix string, tls *types.TLSConfig) {
	if tls == nil || !tls.Enabled {
		return
	}

	if tls.CertFile == "" {
		ve.addf("%s: certFile is required when TLS is enabled", prefix)
	} else if strings.Contains(tls.CertFile, "..") {
		ve.addf("%s: certFile contains path traversal", prefix)
	} else if _, err := os.Stat(tls.CertFile); err != nil {
		ve.addf("%s: certFile %q: %v", prefix, tls.CertFile, err)
	}

	if tls.KeyFile == "" {
		ve.addf("%s: keyFile is required when TLS is enabled", prefix)
	} else if strings.Contains(tls.KeyFile, "..") {
		ve.addf("%s: keyFile contains path traversal", prefix)
	} else if _, err := os.Stat(tls.KeyFile); err != nil {
		ve.addf("%s: keyFile %q: %v", prefix, tls.KeyFile, err)
	}

	if tls.CAFile != "" {
		if strings.Contains(tls.CAFile, "..") {
			ve.addf("%s: caFile contains path traversal", prefix)
		} else if _, err := os.Stat(tls.CAFile); err != nil {
			ve.addf("%s: caFile %q: %v", prefix, tls.CAFile, err)
		}
	}
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

	// Validate SecretsFile exists and is readable if set.
	if ds.Cluster != nil && ds.Cluster.Spec.SecretsFile != "" {
		if strings.Contains(ds.Cluster.Spec.SecretsFile, "..") {
			ve.add("spec.secretsFile contains path traversal")
		} else if _, err := os.Stat(ds.Cluster.Spec.SecretsFile); err != nil {
			ve.addf("spec.secretsFile %q: %v", ds.Cluster.Spec.SecretsFile, err)
		}
	}

	// Rule 13: Model provider names must not shadow reserved provider names.
	// Also validate that model names are non-empty.
	if ds.Cluster != nil {
		for i, model := range ds.Cluster.Spec.Models {
			if model.Name == "" {
				ve.addf("spec.models[%d]: name is required", i)
				continue
			}
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
	if agent.APIVersion != apiVersionV1 {
		ve.addf("%s: apiVersion must be \"hive/v1\", got %q", prefix, agent.APIVersion)
	}
	if agent.Kind != "Agent" {
		ve.addf("%s: kind must be \"Agent\", got %q", prefix, agent.Kind)
	}

	// Validate metadata.id
	if agent.Metadata.ID == "" {
		ve.addf("%s: metadata.id is required", prefix)
	} else if !hiveIDRegex.MatchString(agent.Metadata.ID) {
		ve.addf("%s: metadata.id %q does not match pattern [a-z0-9][a-z0-9-]{0,62}", prefix, agent.Metadata.ID)
	} else if err := types.ValidateSubjectComponent("metadata.id", agent.Metadata.ID); err != nil {
		ve.addf("%s: %v", prefix, err)
	}

	// Validate metadata.team references existing team
	if agent.Metadata.Team != "" {
		if _, ok := teams[agent.Metadata.Team]; !ok {
			ve.addf("%s: metadata.team %q references nonexistent team", prefix, agent.Metadata.Team)
		}
	}

	// Validate runtime.type is required
	validRuntimeTypes := map[string]bool{
		"openclaw": true, "custom": true, "process": true,
	}
	if agent.Spec.Runtime.Type == "" {
		ve.addf("%s: spec.runtime.type is required", prefix)
	} else if !validRuntimeTypes[agent.Spec.Runtime.Type] {
		ve.addf("%s: spec.runtime.type must be one of [openclaw, custom, process], got %q", prefix, agent.Spec.Runtime.Type)
	}

	// Validate runtime.backend
	if agent.Spec.Runtime.Backend != "" {
		validBackends := map[string]bool{"firecracker": true, "process": true}
		if !validBackends[agent.Spec.Runtime.Backend] {
			ve.addf("%s: spec.runtime.backend must be one of [firecracker, process], got %q", prefix, agent.Spec.Runtime.Backend)
		}
	}

	// Validate runtime.command is required for process and custom types
	if agent.Spec.Runtime.Type == "process" || agent.Spec.Runtime.Type == "custom" {
		if agent.Spec.Runtime.Command == "" {
			ve.addf("%s: spec.runtime.command is required when runtime.type is %q", prefix, agent.Spec.Runtime.Type)
		}
	}

	// Validate tier
	if agent.Spec.Tier != "" {
		validTiers := map[string]bool{"vm": true, "native": true}
		if !validTiers[agent.Spec.Tier] {
			ve.addf("%s: spec.tier must be one of [vm, native], got %q", prefix, agent.Spec.Tier)
		}
	}

	// Validate mode
	if agent.Spec.Mode != "" {
		validModes := map[string]bool{types.AgentModeManaged: true, types.AgentModeExternal: true}
		if !validModes[agent.Spec.Mode] {
			ve.addf("%s: spec.mode must be one of [managed, external], got %q", prefix, agent.Spec.Mode)
		}
		// Cross-validate: vm tier cannot be external (hived always manages VMs).
		if agent.Spec.Tier == "vm" && agent.Spec.Mode == types.AgentModeExternal {
			ve.addf("%s: spec.mode \"external\" is invalid for tier \"vm\" (VMs are always managed by hived)", prefix)
		}
	}

	// Validate resources
	if agent.Spec.Resources.Memory != "" {
		if _, err := ParseMemory(agent.Spec.Resources.Memory); err != nil {
			ve.addf("%s: spec.resources.memory %q is invalid: %v", prefix, agent.Spec.Resources.Memory, err)
		}
	}
	if agent.Spec.Resources.VCPUs < 0 {
		ve.addf("%s: spec.resources.vcpus must be non-negative, got %d", prefix, agent.Spec.Resources.VCPUs)
	}

	// Validate capabilities (rule 7: unique names within agent)
	validParamTypes := map[string]bool{"string": true, "int": true, "float": true, "bool": true, "bytes": true}
	capNames := make(map[string]bool)
	for _, cap := range agent.Spec.Capabilities {
		if cap.Name == "" {
			ve.addf("%s: capability name is required", prefix)
			continue
		}
		if err := types.ValidateSubjectComponent("capability name", cap.Name); err != nil {
			ve.addf("%s: capability %q: %v", prefix, cap.Name, err)
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

	// Validate mounts.
	if len(agent.Spec.Mounts) > 0 {
		mountNames := make(map[string]bool)
		rwGuests := make(map[string]bool)
		validMountModes := map[string]bool{"ro": true, "rw": true}
		for _, m := range agent.Spec.Mounts {
			if m.Name == "" {
				ve.addf("%s: mount name is required", prefix)
			} else if mountNames[m.Name] {
				ve.addf("%s: duplicate mount name %q", prefix, m.Name)
			}
			mountNames[m.Name] = true
			if m.Host == "" {
				ve.addf("%s: mount %q host path is required", prefix, m.Name)
			} else if strings.Contains(m.Host, "..") {
				ve.addf("%s: mount %q host path %q contains path traversal (..)", prefix, m.Name, m.Host)
			}
			if m.Guest == "" {
				ve.addf("%s: mount %q guest path is required", prefix, m.Name)
			} else {
				if dangerousGuestPaths[m.Guest] {
					ve.addf("%s: mount %q guest path %q is a dangerous system path", prefix, m.Name, m.Guest)
				}
				if strings.Contains(m.Guest, "..") {
					ve.addf("%s: mount %q guest path %q contains path traversal (..)", prefix, m.Name, m.Guest)
				}
			}
			if m.Mode != "" && !validMountModes[m.Mode] {
				ve.addf("%s: mount %q mode must be one of [ro, rw], got %q", prefix, m.Name, m.Mode)
			}
			// Check for overlapping rw mounts on the same guest path.
			if m.Mode == "rw" && m.Guest != "" {
				if rwGuests[m.Guest] {
					ve.addf("%s: mount %q overlapping rw mount on guest path %q", prefix, m.Name, m.Guest)
				}
				rwGuests[m.Guest] = true
			}
		}
	}

	// Validate secrets reference existing cluster secrets.
	if len(agent.Spec.Secrets) > 0 {
		for _, s := range agent.Spec.Secrets {
			if s.Name == "" {
				ve.addf("%s: secret name is required", prefix)
				continue
			}
			if s.Env == "" {
				ve.addf("%s: secret %q env is required", prefix, s.Name)
			}
			// Cross-reference against cluster secrets.
			if cluster != nil {
				if cluster.Spec.Secrets == nil {
					ve.addf("%s: secret %q referenced but no secrets defined in cluster config", prefix, s.Name)
				} else if _, ok := cluster.Spec.Secrets[s.Name]; !ok {
					ve.addf("%s: secret %q not found in cluster spec.secrets", prefix, s.Name)
				}
			}
		}
	}

	// Validate replicas.
	if agent.Spec.Replicas.Min < 0 {
		ve.addf("%s: spec.replicas.min must be >= 0", prefix)
	}
	if agent.Spec.Replicas.Max < 0 {
		ve.addf("%s: spec.replicas.max must be >= 0", prefix)
	}
	if agent.Spec.Replicas.Max > 10000 {
		ve.addf("%s: spec.replicas.max must be <= 10000, got %d", prefix, agent.Spec.Replicas.Max)
	}
	if agent.Spec.Replicas.Min > 0 && agent.Spec.Replicas.Max > 0 && agent.Spec.Replicas.Min > agent.Spec.Replicas.Max {
		ve.addf("%s: spec.replicas.min (%d) must be <= max (%d)", prefix, agent.Spec.Replicas.Min, agent.Spec.Replicas.Max)
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

	// Validate hardware fields (GPU, sensors, actuators).
	if agent.Spec.Hardware.GPU != "" {
		if strings.ContainsAny(agent.Spec.Hardware.GPU, "/\\") {
			ve.addf("%s: spec.hardware.gpu %q must not contain path separators", prefix, agent.Spec.Hardware.GPU)
		}
		for _, c := range agent.Spec.Hardware.GPU {
			if c < 0x20 || c == 0x7f {
				ve.addf("%s: spec.hardware.gpu %q contains control characters", prefix, agent.Spec.Hardware.GPU)
				break
			}
		}
	}
	for _, sensor := range agent.Spec.Hardware.Sensors {
		if strings.ContainsAny(sensor, "/\\") {
			ve.addf("%s: spec.hardware.sensors entry %q must not contain path separators", prefix, sensor)
		}
		for _, c := range sensor {
			if c < 0x20 || c == 0x7f {
				ve.addf("%s: spec.hardware.sensors entry %q contains control characters", prefix, sensor)
				break
			}
		}
	}
	for _, actuator := range agent.Spec.Hardware.Actuators {
		if strings.ContainsAny(actuator, "/\\") {
			ve.addf("%s: spec.hardware.actuators entry %q must not contain path separators", prefix, actuator)
		}
		for _, c := range actuator {
			if c < 0x20 || c == 0x7f {
				ve.addf("%s: spec.hardware.actuators entry %q contains control characters", prefix, actuator)
				break
			}
		}
	}

	// Validate restart policy
	if agent.Spec.Restart.Policy != "" {
		validPolicies := map[string]bool{"always": true, restartOnFailure: true, "never": true}
		if !validPolicies[agent.Spec.Restart.Policy] {
			ve.addf("%s: spec.restart.policy must be one of [always, on-failure, never], got %q", prefix, agent.Spec.Restart.Policy)
		}
	}

	// Validate ingress configuration.
	if agent.Spec.Ingress.Port != 0 || agent.Spec.Ingress.Path != "" {
		if agent.Spec.Ingress.Port <= 0 || agent.Spec.Ingress.Port > 65535 {
			ve.addf("%s: spec.ingress.port must be between 1 and 65535, got %d", prefix, agent.Spec.Ingress.Port)
		}
		if agent.Spec.Ingress.Path != "" {
			if agent.Spec.Ingress.Path[0] != '/' {
				ve.addf("%s: spec.ingress.path must start with '/', got %q", prefix, agent.Spec.Ingress.Path)
			}
			if strings.Contains(agent.Spec.Ingress.Path, "..") {
				ve.addf("%s: spec.ingress.path %q contains path traversal (..)", prefix, agent.Spec.Ingress.Path)
			}
			if ingressPathInvalidChars.MatchString(agent.Spec.Ingress.Path) {
				ve.addf("%s: spec.ingress.path %q contains invalid characters", prefix, agent.Spec.Ingress.Path)
			}
		}
	}

	// Validate disk size.
	if agent.Spec.Resources.Disk != "" {
		diskBytes, err := ParseDiskSize(agent.Spec.Resources.Disk)
		if err != nil {
			ve.addf("%s: spec.resources.disk %q is invalid: %v", prefix, agent.Spec.Resources.Disk, err)
		} else if diskBytes > 10*1024*1024*1024*1024 { // 10 TB upper bound
			ve.addf("%s: spec.resources.disk %q exceeds maximum of 10TB", prefix, agent.Spec.Resources.Disk)
		}
	}

	// Cap environment variables per agent to prevent excessive resource usage.
	if len(agent.Spec.Runtime.Model.Env) > 1000 {
		ve.addf("%s: spec.runtime.model.env has %d entries, maximum is 1000", prefix, len(agent.Spec.Runtime.Model.Env))
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
	// NOTE: Uses global slog logger. A future refactor could accept a logger
	// parameter, but that would change many call signatures throughout the
	// validation chain.
	if agent.Spec.Placement.NodeID != "" {
		slog.Warn("agent placement.nodeId is set; node registration cannot be verified at parse time",
			"agent_id", id, "node_id", agent.Spec.Placement.NodeID)
	}
}

func validateTeam(ve *ValidationError, id string, team *types.TeamManifest, agents map[string]*types.AgentManifest) {
	prefix := fmt.Sprintf("team %q", id)

	if team.APIVersion != apiVersionV1 {
		ve.addf("%s: apiVersion must be \"hive/v1\", got %q", prefix, team.APIVersion)
	}
	if team.Kind != "Team" {
		ve.addf("%s: kind must be \"Team\", got %q", prefix, team.Kind)
	}

	if team.Metadata.ID == "" {
		ve.addf("%s: metadata.id is required", prefix)
	} else if !hiveIDRegex.MatchString(team.Metadata.ID) {
		ve.addf("%s: metadata.id %q does not match pattern [a-z0-9][a-z0-9-]{0,62}", prefix, team.Metadata.ID)
	} else if err := types.ValidateSubjectComponent("metadata.id", team.Metadata.ID); err != nil {
		ve.addf("%s: %v", prefix, err)
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

	// Validate MaxAgents bounds.
	if team.Spec.Resources.MaxAgents < 0 || team.Spec.Resources.MaxAgents > 10000 {
		ve.addf("%s: spec.resources.maxAgents must be between 0 and 10000, got %d", prefix, team.Spec.Resources.MaxAgents)
	}

	// Validate HistoryDepth bounds.
	if team.Spec.Communication.HistoryDepth < 0 || team.Spec.Communication.HistoryDepth > 100000 {
		ve.addf("%s: spec.communication.historyDepth must be between 0 and 100000, got %d", prefix, team.Spec.Communication.HistoryDepth)
	}

	// Validate team MaxMemory if set.
	if team.Spec.Resources.MaxMemory != "" {
		if _, err := ParseMemory(team.Spec.Resources.MaxMemory); err != nil {
			ve.addf("%s: spec.resources.maxMemory %q is invalid: %v", prefix, team.Spec.Resources.MaxMemory, err)
		}
	}

	// Validate communication namespace for NATS safety.
	if team.Spec.Communication.Namespace != "" {
		if err := types.ValidateSubjectComponent("communication.namespace", team.Spec.Communication.Namespace); err != nil {
			ve.addf("%s: %v", prefix, err)
		}
	}

	// Validate shared volume name uniqueness.
	volumeNames := make(map[string]bool)
	for _, vol := range team.Spec.SharedVolumes {
		if volumeNames[vol.Name] {
			ve.addf("%s: duplicate shared volume name: %s", prefix, vol.Name)
		}
		volumeNames[vol.Name] = true
	}

	// Validate shared_volumes access values and host path safety.
	validSharedVolumeAccess := map[string]bool{"read-only": true, "read-write": true}
	for _, sv := range team.Spec.SharedVolumes {
		if sv.Access != "" && !validSharedVolumeAccess[sv.Access] {
			ve.addf("%s: shared_volume %q access must be one of [read-only, read-write], got %q", prefix, sv.Name, sv.Access)
		}
		if sv.HostPath != "" && strings.Contains(sv.HostPath, "..") {
			ve.addf("%s: shared_volume %q hostPath %q contains path traversal (..)", prefix, sv.Name, sv.HostPath)
		}
	}
}

// ParseMemory parses a memory string like "512Mi", "1Gi", "256MB" into bytes.
// Note: Uses float64 arithmetic internally, which may lose precision for values
// exceeding 2^53 bytes (~8 PiB). For typical memory sizes this is not an issue.
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

	// Reject leading zeros (e.g., "09Mi") but allow "0" and "0.5".
	if len(numStr) > 1 && numStr[0] == '0' && numStr[1] != '.' {
		return 0, fmt.Errorf("invalid memory format %q: leading zeros not allowed", s)
	}

	// Reject multiple decimal points.
	if strings.Count(numStr, ".") > 1 {
		return 0, fmt.Errorf("invalid memory format %q: multiple decimal points", s)
	}

	var num float64
	if _, err := fmt.Sscanf(numStr, "%f", &num); err != nil {
		return 0, fmt.Errorf("invalid memory format %q: %w", s, err)
	}
	if math.IsNaN(num) || math.IsInf(num, 0) {
		return 0, fmt.Errorf("invalid numeric value in %q", s)
	}
	if num <= 0 {
		return 0, fmt.Errorf("invalid memory %q: must be positive", s)
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

	result := int64(num * mult)

	// Reject values that truncate to zero due to float64 rounding.
	if result <= 0 {
		return 0, fmt.Errorf("invalid memory size %q: effective size is zero bytes", s)
	}

	// Reject values exceeding float64 safe integer boundary (2^53).
	if num*mult > float64(1<<53) {
		return 0, fmt.Errorf("invalid memory %q: value too large for precise representation", s)
	}

	return result, nil
}

// ParseDiskSize parses a disk size string like "1G", "512M", "100Gi" into bytes.
// Note: Uses float64 arithmetic internally, which may lose precision for values
// exceeding 2^53 bytes (~8 PiB). For typical disk sizes this is not an issue.
func ParseDiskSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty disk size string")
	}

	// Find where the numeric part ends.
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0, fmt.Errorf("invalid disk size format %q: no numeric value", s)
	}

	numStr := s[:i]
	suffix := s[i:]

	// Reject leading zeros (e.g., "09Gi") but allow "0" and "0.5".
	if len(numStr) > 1 && numStr[0] == '0' && numStr[1] != '.' {
		return 0, fmt.Errorf("invalid disk size format %q: leading zeros not allowed", s)
	}

	// Reject multiple decimal points.
	if strings.Count(numStr, ".") > 1 {
		return 0, fmt.Errorf("invalid disk size format %q: multiple decimal points", s)
	}

	var num float64
	if _, err := fmt.Sscanf(numStr, "%f", &num); err != nil {
		return 0, fmt.Errorf("invalid disk size format %q: %w", s, err)
	}
	if math.IsNaN(num) || math.IsInf(num, 0) {
		return 0, fmt.Errorf("invalid numeric value in %q", s)
	}
	if num <= 0 {
		return 0, fmt.Errorf("invalid disk size %q: must be positive", s)
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
		"T":  1024 * 1024 * 1024 * 1024,
		"Ti": 1024 * 1024 * 1024 * 1024,
		"TB": 1000 * 1000 * 1000 * 1000,
	}

	mult, ok := multipliers[suffix]
	if !ok {
		return 0, fmt.Errorf("invalid disk size suffix %q in %q", suffix, s)
	}

	result := int64(num * mult)

	// Reject values that truncate to zero due to float64 rounding.
	if result <= 0 {
		return 0, fmt.Errorf("invalid disk size %q: effective size is zero bytes", s)
	}

	// Reject values exceeding float64 safe integer boundary (2^53).
	if num*mult > float64(1<<53) {
		return 0, fmt.Errorf("invalid disk size %q: value too large for precise representation", s)
	}

	return result, nil
}
