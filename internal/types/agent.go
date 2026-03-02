// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package types defines shared data structures, envelopes, and validation helpers used across Hive packages.
package types

import "time"

// Agent lifecycle mode constants.
const (
	AgentModeManaged  = "managed"  // hived manages the agent process
	AgentModeExternal = "external" // agent joins externally via hive-agent
)

// ResolvedMode returns the effective lifecycle mode for this agent spec.
// If Mode is explicitly set, it is returned as-is. Otherwise, the mode is
// inferred from the spec fields:
//   - tier "vm" or runtime.backend "firecracker" → managed
//   - runtime.command is set → managed
//   - tier "native" with no command → external
//   - otherwise (including empty tier) → managed
//
// This matches the legacy heuristic (tier == "native" && command == "")
// so that existing manifests without an explicit mode behave identically.
// This method does NOT mutate the Mode field, preserving manifest hash stability.
func (s AgentSpec) ResolvedMode() string {
	if s.Mode != "" {
		return s.Mode
	}
	if s.Tier == "vm" || s.Runtime.Backend == "firecracker" {
		return AgentModeManaged
	}
	if s.Runtime.Command != "" {
		return AgentModeManaged
	}
	if s.Tier == "native" {
		return AgentModeExternal
	}
	return AgentModeManaged
}

// IsManaged returns true if hived is responsible for starting and managing
// this agent's process lifecycle.
func (s AgentSpec) IsManaged() bool {
	return s.ResolvedMode() == AgentModeManaged
}

// AgentManifest represents the parsed agent manifest (agents/AGENT_ID/manifest.yaml).
type AgentManifest struct {
	APIVersion string        `yaml:"apiVersion" json:"apiVersion"`
	Kind       string        `yaml:"kind" json:"kind"`
	Metadata   AgentMetadata `yaml:"metadata" json:"metadata"`
	Spec       AgentSpec     `yaml:"spec" json:"spec"`
}

type AgentMetadata struct {
	ID     string            `yaml:"id" json:"id"`
	Team   string            `yaml:"team,omitempty" json:"team,omitempty"`
	Labels map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
}

type AgentSpec struct {
	Tier         string            `yaml:"tier,omitempty" json:"tier,omitempty"`
	Mode         string            `yaml:"mode,omitempty" json:"mode,omitempty"`
	Resources    AgentResources    `yaml:"resources,omitempty" json:"resources,omitempty"`
	Runtime      AgentRuntime      `yaml:"runtime" json:"runtime"`
	Capabilities []AgentCapability `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	Network      AgentNetwork      `yaml:"network,omitempty" json:"network,omitempty"`
	Volumes      []AgentVolume     `yaml:"volumes,omitempty" json:"volumes,omitempty"`
	Mounts       []AgentMount      `yaml:"mounts,omitempty" json:"mounts,omitempty"`
	Secrets      []AgentSecret     `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	Replicas     AgentReplicas     `yaml:"replicas,omitempty" json:"replicas,omitempty"`
	Health       AgentHealth       `yaml:"health,omitempty" json:"health,omitempty"`
	Restart      AgentRestart      `yaml:"restart,omitempty" json:"restart,omitempty"`
	Placement    AgentPlacement    `yaml:"placement,omitempty" json:"placement,omitempty"`
	Hardware     AgentHardware     `yaml:"hardware,omitempty" json:"hardware,omitempty"`
	Ingress      AgentIngress      `yaml:"ingress,omitempty" json:"ingress,omitempty"`
}

type AgentResources struct {
	Memory string `yaml:"memory,omitempty" json:"memory,omitempty"`
	VCPUs  int    `yaml:"vcpus,omitempty" json:"vcpus,omitempty"`
	Disk   string `yaml:"disk,omitempty" json:"disk,omitempty"`
}

type AgentRuntime struct {
	Type    string     `yaml:"type" json:"type"`
	Backend string     `yaml:"backend,omitempty" json:"backend,omitempty"` // "firecracker" or "process"
	Command string     `yaml:"command,omitempty" json:"command,omitempty"` // command for custom/process runtimes
	Image   string     `yaml:"image,omitempty" json:"image,omitempty"`     // rootfs image profile name
	Model   AgentModel `yaml:"model,omitempty" json:"model,omitempty"`
}

type AgentModel struct {
	Provider string            `yaml:"provider,omitempty" json:"provider,omitempty"`
	Name     string            `yaml:"name,omitempty" json:"name,omitempty"`
	Env      map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
}

type AgentCapability struct {
	Name        string            `yaml:"name" json:"name"`
	Description string            `yaml:"description" json:"description"`
	Inputs      []CapabilityParam `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	Outputs     []CapabilityParam `yaml:"outputs,omitempty" json:"outputs,omitempty"`
	Async       bool              `yaml:"async,omitempty" json:"async,omitempty"`
}

type CapabilityParam struct {
	Name        string `yaml:"name" json:"name"`
	Type        string `yaml:"type" json:"type"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Required    *bool  `yaml:"required,omitempty" json:"required,omitempty"`
}

// IsRequired returns whether the parameter is required (default true).
func (p CapabilityParam) IsRequired() bool {
	if p.Required == nil {
		return true
	}
	return *p.Required
}

type AgentNetwork struct {
	Egress          string   `yaml:"egress,omitempty" json:"egress,omitempty"`
	EgressAllowlist []string `yaml:"egress_allowlist,omitempty" json:"egress_allowlist,omitempty"`
	Ingress         string   `yaml:"ingress,omitempty" json:"ingress,omitempty"` // "none", "restricted", "full"
}

// AgentIngress defines ingress routing configuration for an agent.
type AgentIngress struct {
	Port int    `yaml:"port,omitempty" json:"port,omitempty"`
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
}

type AgentVolume struct {
	Name      string `yaml:"name" json:"name"`
	MountPath string `yaml:"mountPath" json:"mountPath"`
	Access    string `yaml:"access,omitempty" json:"access,omitempty"`
}

type AgentHealth struct {
	Enabled     *bool         `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Interval    time.Duration `yaml:"interval,omitempty" json:"interval,omitempty"`
	Timeout     time.Duration `yaml:"timeout,omitempty" json:"timeout,omitempty"`
	MaxFailures int           `yaml:"maxFailures,omitempty" json:"maxFailures,omitempty"`
}

// IsEnabled returns whether health checks are enabled (default true).
func (h AgentHealth) IsEnabled() bool {
	if h.Enabled == nil {
		return true
	}
	return *h.Enabled
}

type AgentRestart struct {
	Policy      string        `yaml:"policy,omitempty" json:"policy,omitempty"`
	MaxRestarts int           `yaml:"maxRestarts,omitempty" json:"maxRestarts,omitempty"`
	Backoff     time.Duration `yaml:"backoff,omitempty" json:"backoff,omitempty"`
}

type AgentPlacement struct {
	NodeID     string            `yaml:"nodeId,omitempty" json:"nodeId,omitempty"`
	NodeLabels map[string]string `yaml:"nodeLabels,omitempty" json:"nodeLabels,omitempty"`
	Arch       string            `yaml:"arch,omitempty" json:"arch,omitempty"`
}

type AgentHardware struct {
	GPIO      bool              `yaml:"gpio,omitempty" json:"gpio,omitempty"`
	Camera    bool              `yaml:"camera,omitempty" json:"camera,omitempty"`
	Sensors   []string          `yaml:"sensors,omitempty" json:"sensors,omitempty"`
	Actuators []string          `yaml:"actuators,omitempty" json:"actuators,omitempty"`
	GPU       string            `yaml:"gpu,omitempty" json:"gpu,omitempty"`
	Custom    map[string]string `yaml:"custom,omitempty" json:"custom,omitempty"`
}

// AgentMount defines a host-to-guest filesystem mount.
type AgentMount struct {
	Name  string `yaml:"name" json:"name"`
	Host  string `yaml:"host" json:"host"`
	Guest string `yaml:"guest" json:"guest"`
	Mode  string `yaml:"mode" json:"mode"` // "ro" or "rw"
}

// AgentSecret defines a secret to inject into the agent's environment.
type AgentSecret struct {
	Name string `yaml:"name" json:"name"`
	Env  string `yaml:"env" json:"env"`
}

// AgentReplicas defines the desired replica count and scaling behavior.
type AgentReplicas struct {
	Min int `yaml:"min,omitempty" json:"min,omitempty"`
	Max int `yaml:"max,omitempty" json:"max,omitempty"`
}
