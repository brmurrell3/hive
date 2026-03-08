// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package config handles loading and validation of cluster.yaml and agent/team manifests.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/brmurrell3/hive/internal/types"
	"gopkg.in/yaml.v3"
)

// LoadCluster reads and parses cluster.yaml from the given cluster root directory.
func LoadCluster(clusterRoot string) (*types.ClusterConfig, error) {
	path := filepath.Join(clusterRoot, "cluster.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading cluster.yaml: %w", err)
	}
	return ParseCluster(data)
}

// ParseCluster parses cluster.yaml content into a ClusterConfig with defaults applied.
func ParseCluster(data []byte) (*types.ClusterConfig, error) {
	// Parse into raw structure first for duration handling
	var raw rawClusterConfig
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing cluster.yaml: %w", err)
	}

	cfg := &types.ClusterConfig{
		APIVersion: raw.APIVersion,
		Kind:       raw.Kind,
		Metadata:   raw.Metadata,
	}

	// Parse NATS duration fields
	natsReadyTimeout, err := parseDurationOrDefault(raw.Spec.NATS.ReadyTimeout, 0)
	if err != nil {
		return nil, fmt.Errorf("parsing nats.readyTimeout: %w", err)
	}
	natsShutdownTimeout, err := parseDurationOrDefault(raw.Spec.NATS.ShutdownTimeout, 0)
	if err != nil {
		return nil, fmt.Errorf("parsing nats.shutdownTimeout: %w", err)
	}

	// Apply spec
	cfg.Spec.NATS = types.NATSConfig{
		Port:             raw.Spec.NATS.Port,
		ClusterPort:      raw.Spec.NATS.ClusterPort,
		Mode:             raw.Spec.NATS.Mode,
		URLs:             raw.Spec.NATS.URLs,
		ClusterPeers:     raw.Spec.NATS.ClusterPeers,
		AuthToken:        raw.Spec.NATS.AuthToken,
		ClusterAuthToken: raw.Spec.NATS.ClusterAuthToken,
		Host:             raw.Spec.NATS.Host,
		TLS:              raw.Spec.NATS.TLS,
		MaxConnections:   raw.Spec.NATS.MaxConnections,
		MaxSubscriptions: raw.Spec.NATS.MaxSubscriptions,
		ClusterName:      raw.Spec.NATS.ClusterName,
		ReadyTimeout:     natsReadyTimeout,
		ShutdownTimeout:  natsShutdownTimeout,
		JetStream: types.JetStreamConfig{
			Enabled:    raw.Spec.NATS.JetStream.Enabled,
			StorePath:  raw.Spec.NATS.JetStream.StorePath,
			MaxMemory:  raw.Spec.NATS.JetStream.MaxMemory,
			MaxStorage: raw.Spec.NATS.JetStream.MaxStorage,
		},
	}

	// Parse durations for defaults
	healthInterval, err := parseDurationOrDefault(raw.Spec.Defaults.Health.Interval, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("parsing defaults.health.interval: %w", err)
	}
	healthTimeout, err := parseDurationOrDefault(raw.Spec.Defaults.Health.Timeout, 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("parsing defaults.health.timeout: %w", err)
	}
	restartBackoff, err := parseDurationOrDefault(raw.Spec.Defaults.Restart.Backoff, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("parsing defaults.restart.backoff: %w", err)
	}

	cfg.Spec.Defaults = types.DefaultsConfig{
		Resources: raw.Spec.Defaults.Resources,
		Health: types.HealthConfig{
			Enabled:     raw.Spec.Defaults.Health.Enabled,
			Interval:    healthInterval,
			Timeout:     healthTimeout,
			MaxFailures: raw.Spec.Defaults.Health.MaxFailures,
		},
		Restart: types.RestartConfig{
			Policy:      raw.Spec.Defaults.Restart.Policy,
			MaxRestarts: raw.Spec.Defaults.Restart.MaxRestarts,
			Backoff:     restartBackoff,
		},
	}

	// Parse remaining spec fields.
	cfg.Spec.Dashboard = raw.Spec.Dashboard
	cfg.Spec.Metrics = raw.Spec.Metrics
	cfg.Spec.Logging = raw.Spec.Logging
	cfg.Spec.Secrets = raw.Spec.Secrets
	cfg.Spec.SecretsFile = raw.Spec.SecretsFile
	cfg.Spec.Models = raw.Spec.Models
	cfg.Spec.Nodes = raw.Spec.Nodes
	cfg.Spec.VM = raw.Spec.VM
	cfg.Spec.Users = raw.Spec.Users

	applyDefaults(cfg)

	if err := validateCluster(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// rawClusterConfig mirrors ClusterConfig but uses strings for durations.
type rawClusterConfig struct {
	APIVersion string                `yaml:"apiVersion"`
	Kind       string                `yaml:"kind"`
	Metadata   types.ClusterMetadata `yaml:"metadata"`
	Spec       rawClusterSpec        `yaml:"spec"`
}

type rawClusterSpec struct {
	NATS        rawNATSConfig         `yaml:"nats"`
	Defaults    rawDefaultsConfig     `yaml:"defaults"`
	Dashboard   types.DashboardConfig `yaml:"dashboard"`
	Metrics     types.MetricsConfig   `yaml:"metrics"`
	Logging     types.LoggingConfig   `yaml:"logging"`
	Secrets     map[string]string     `yaml:"secrets"`
	SecretsFile string                `yaml:"secretsFile"`
	Models      []types.ModelConfig   `yaml:"models"`
	Nodes       types.NodeConfig      `yaml:"nodes"`
	VM          types.VMConfig        `yaml:"vm"`
	Users       []types.UserConfig    `yaml:"users"`
}

type rawNATSConfig struct {
	Port             int                `yaml:"port"`
	ClusterPort      int                `yaml:"clusterPort"`
	JetStream        rawJetStreamConfig `yaml:"jetstream"`
	Mode             string             `yaml:"mode"`
	URLs             []string           `yaml:"urls"`
	ClusterPeers     []string           `yaml:"clusterPeers"`
	AuthToken        string             `yaml:"authToken"`
	ClusterAuthToken string             `yaml:"clusterAuthToken"`
	Host             string             `yaml:"host"`
	TLS              types.TLSConfig    `yaml:"tls"`
	MaxConnections   int                `yaml:"maxConnections"`
	MaxSubscriptions int                `yaml:"maxSubscriptions"`
	ClusterName      string             `yaml:"clusterName"`
	ReadyTimeout     string             `yaml:"readyTimeout"`
	ShutdownTimeout  string             `yaml:"shutdownTimeout"`
}

type rawJetStreamConfig struct {
	Enabled    *bool  `yaml:"enabled"`
	StorePath  string `yaml:"storePath"`
	MaxMemory  string `yaml:"maxMemory"`
	MaxStorage string `yaml:"maxStorage"`
}

type rawDefaultsConfig struct {
	Resources types.ResourceDefaults `yaml:"resources"`
	Health    rawHealthConfig        `yaml:"health"`
	Restart   rawRestartConfig       `yaml:"restart"`
}

type rawHealthConfig struct {
	Enabled     *bool  `yaml:"enabled"`
	Interval    string `yaml:"interval"`
	Timeout     string `yaml:"timeout"`
	MaxFailures int    `yaml:"maxFailures"`
}

type rawRestartConfig struct {
	Policy      string `yaml:"policy"`
	MaxRestarts int    `yaml:"maxRestarts"`
	Backoff     string `yaml:"backoff"`
}

func parseDurationOrDefault(s string, def time.Duration) (time.Duration, error) {
	if s == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", s, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("duration must be non-negative, got %s", s)
	}
	return d, nil
}

// LoadAgents reads and parses all agent manifests from agents/AGENT_ID/manifest.yaml
// under the given cluster root directory. Returns a map keyed by agent ID.
func LoadAgents(clusterRoot string) (map[string]*types.AgentManifest, error) {
	agentsDir := filepath.Join(clusterRoot, "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]*types.AgentManifest{}, nil
		}
		return nil, fmt.Errorf("reading agents directory: %w", err)
	}

	agents := make(map[string]*types.AgentManifest)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// Support both .yaml and .yml extensions for agent manifests.
		manifestPath := filepath.Join(agentsDir, entry.Name(), "manifest.yaml")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			if os.IsNotExist(err) {
				// Try .yml extension as fallback.
				manifestPath = filepath.Join(agentsDir, entry.Name(), "manifest.yml")
				data, err = os.ReadFile(manifestPath)
				if err != nil {
					if os.IsNotExist(err) {
						slog.Warn("agent directory has no manifest file (manifest.yaml or manifest.yml), skipping",
							"directory", entry.Name(),
							"path", filepath.Join(agentsDir, entry.Name()))
						continue
					}
					return nil, fmt.Errorf("reading agent manifest %s: %w", entry.Name(), err)
				}
			} else {
				return nil, fmt.Errorf("reading agent manifest %s: %w", entry.Name(), err)
			}
		}

		agent, err := ParseAgent(data)
		if err != nil {
			return nil, fmt.Errorf("parsing agent manifest %s: %w", entry.Name(), err)
		}

		// Detect duplicate agent IDs.
		if _, dup := agents[agent.Metadata.ID]; dup {
			return nil, fmt.Errorf("duplicate agent ID %q: found in %s and another directory", agent.Metadata.ID, entry.Name())
		}
		agents[agent.Metadata.ID] = agent
	}

	return agents, nil
}

// ParseAgent parses agent manifest YAML content into an AgentManifest.
func ParseAgent(data []byte) (*types.AgentManifest, error) {
	var raw rawAgentManifest
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing agent manifest: %w", err)
	}

	agent := &types.AgentManifest{
		APIVersion: raw.APIVersion,
		Kind:       raw.Kind,
		Metadata:   raw.Metadata,
	}

	// Copy spec fields that don't need duration parsing
	agent.Spec.Tier = raw.Spec.Tier
	agent.Spec.Mode = raw.Spec.Mode
	agent.Spec.Resources = raw.Spec.Resources
	agent.Spec.Runtime = raw.Spec.Runtime

	// Map tier to runtime backend when backend is not explicitly set.
	if agent.Spec.Runtime.Backend == "" && agent.Spec.Tier == "native" {
		agent.Spec.Runtime.Backend = backendProcess
	}
	agent.Spec.Capabilities = raw.Spec.Capabilities
	agent.Spec.Network = raw.Spec.Network
	agent.Spec.Volumes = raw.Spec.Volumes
	agent.Spec.Mounts = raw.Spec.Mounts
	agent.Spec.Secrets = raw.Spec.Secrets
	agent.Spec.Replicas = raw.Spec.Replicas
	agent.Spec.Placement = raw.Spec.Placement
	agent.Spec.Hardware = raw.Spec.Hardware
	agent.Spec.Ingress = raw.Spec.Ingress

	// Parse health durations
	healthInterval, err := parseDurationOrDefault(raw.Spec.Health.Interval, 0)
	if err != nil {
		return nil, fmt.Errorf("parsing spec.health.interval: %w", err)
	}
	healthTimeout, err := parseDurationOrDefault(raw.Spec.Health.Timeout, 0)
	if err != nil {
		return nil, fmt.Errorf("parsing spec.health.timeout: %w", err)
	}
	agent.Spec.Health = types.AgentHealth{
		Enabled:     raw.Spec.Health.Enabled,
		Interval:    healthInterval,
		Timeout:     healthTimeout,
		MaxFailures: raw.Spec.Health.MaxFailures,
	}

	// Parse restart backoff duration
	restartBackoff, err := parseDurationOrDefault(raw.Spec.Restart.Backoff, 0)
	if err != nil {
		return nil, fmt.Errorf("parsing spec.restart.backoff: %w", err)
	}
	agent.Spec.Restart = types.AgentRestart{
		Policy:      raw.Spec.Restart.Policy,
		MaxRestarts: raw.Spec.Restart.MaxRestarts,
		Backoff:     restartBackoff,
	}

	return agent, nil
}

// rawAgentManifest mirrors AgentManifest but uses strings for durations.
type rawAgentManifest struct {
	APIVersion string              `yaml:"apiVersion"`
	Kind       string              `yaml:"kind"`
	Metadata   types.AgentMetadata `yaml:"metadata"`
	Spec       rawAgentSpec        `yaml:"spec"`
}

type rawAgentSpec struct {
	Tier         string                  `yaml:"tier"`
	Mode         string                  `yaml:"mode"`
	Resources    types.AgentResources    `yaml:"resources"`
	Runtime      types.AgentRuntime      `yaml:"runtime"`
	Capabilities []types.AgentCapability `yaml:"capabilities"`
	Network      types.AgentNetwork      `yaml:"network"`
	Volumes      []types.AgentVolume     `yaml:"volumes"`
	Mounts       []types.AgentMount      `yaml:"mounts"`
	Secrets      []types.AgentSecret     `yaml:"secrets"`
	Replicas     types.AgentReplicas     `yaml:"replicas"`
	Health       rawAgentHealth          `yaml:"health"`
	Restart      rawAgentRestart         `yaml:"restart"`
	Placement    types.AgentPlacement    `yaml:"placement"`
	Hardware     types.AgentHardware     `yaml:"hardware"`
	Ingress      types.AgentIngress      `yaml:"ingress"`
}

type rawAgentHealth struct {
	Enabled     *bool  `yaml:"enabled"`
	Interval    string `yaml:"interval"`
	Timeout     string `yaml:"timeout"`
	MaxFailures int    `yaml:"maxFailures"`
}

type rawAgentRestart struct {
	Policy      string `yaml:"policy"`
	MaxRestarts int    `yaml:"maxRestarts"`
	Backoff     string `yaml:"backoff"`
}

// LoadTeams reads and parses all team manifests from teams/TEAM_ID.yaml
// under the given cluster root directory. Returns a map keyed by team ID.
func LoadTeams(clusterRoot string) (map[string]*types.TeamManifest, error) {
	teamsDir := filepath.Join(clusterRoot, "teams")
	entries, err := os.ReadDir(teamsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]*types.TeamManifest{}, nil
		}
		return nil, fmt.Errorf("reading teams directory: %w", err)
	}

	teams := make(map[string]*types.TeamManifest)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		ext := filepath.Ext(entry.Name())
		if ext != ".yaml" && ext != ".yml" {
			continue
		}

		teamPath := filepath.Join(teamsDir, entry.Name())
		data, err := os.ReadFile(teamPath)
		if err != nil {
			return nil, fmt.Errorf("reading team manifest %s: %w", entry.Name(), err)
		}

		team, err := ParseTeam(data)
		if err != nil {
			return nil, fmt.Errorf("parsing team manifest %s: %w", entry.Name(), err)
		}

		// Detect duplicate team IDs.
		if _, dup := teams[team.Metadata.ID]; dup {
			return nil, fmt.Errorf("duplicate team ID %q: found in %s and another file", team.Metadata.ID, entry.Name())
		}
		teams[team.Metadata.ID] = team
	}

	return teams, nil
}

// ParseTeam parses team manifest YAML content into a TeamManifest.
// Note: This function only performs YAML unmarshalling. Callers must invoke
// validateTeam (via ValidateDesiredState) separately for full validation
// of field values, cross-references, and NATS subject safety.
func ParseTeam(data []byte) (*types.TeamManifest, error) {
	var team types.TeamManifest
	if err := yaml.Unmarshal(data, &team); err != nil {
		return nil, fmt.Errorf("parsing team manifest: %w", err)
	}
	return &team, nil
}

// LoadDesiredState loads the complete desired state from a cluster root directory.
// It reads cluster.yaml, all agent manifests, and all team manifests.
// Merges cluster defaults into agents that omit health/restart fields.
func LoadDesiredState(clusterRoot string) (*types.DesiredState, error) {
	cluster, err := LoadCluster(clusterRoot)
	if err != nil {
		return nil, fmt.Errorf("loading cluster config: %w", err)
	}

	agents, err := LoadAgents(clusterRoot)
	if err != nil {
		return nil, fmt.Errorf("loading agent manifests: %w", err)
	}

	teams, err := LoadTeams(clusterRoot)
	if err != nil {
		return nil, fmt.Errorf("loading team manifests: %w", err)
	}

	// Merge cluster-level defaults into agents that don't set these fields.
	for _, agent := range agents {
		mergeAgentDefaults(agent, cluster)
	}

	return &types.DesiredState{
		Cluster: cluster,
		Agents:  agents,
		Teams:   teams,
	}, nil
}

// mergeAgentDefaults populates agent-level health/restart fields from cluster
// defaults when the agent manifest does not explicitly set them.
// Agents that omit health/restart sections inherit cluster defaults.
//
// Note: Zero values for numeric fields (MaxFailures, MaxRestarts, etc.)
// are treated as "use cluster default". To explicitly set a field to 0,
// use the cluster-level defaults instead.
func mergeAgentDefaults(agent *types.AgentManifest, cluster *types.ClusterConfig) {
	defaults := cluster.Spec.Defaults

	// Health defaults
	if agent.Spec.Health.Enabled == nil && defaults.Health.Enabled != nil {
		val := *defaults.Health.Enabled
		agent.Spec.Health.Enabled = &val
	}
	if agent.Spec.Health.Interval == 0 {
		agent.Spec.Health.Interval = defaults.Health.Interval
	}
	if agent.Spec.Health.Timeout == 0 {
		agent.Spec.Health.Timeout = defaults.Health.Timeout
	}
	if agent.Spec.Health.MaxFailures == 0 {
		agent.Spec.Health.MaxFailures = defaults.Health.MaxFailures
	}

	// Restart defaults
	if agent.Spec.Restart.Policy == "" {
		agent.Spec.Restart.Policy = defaults.Restart.Policy
	}
	if agent.Spec.Restart.Backoff == 0 {
		agent.Spec.Restart.Backoff = defaults.Restart.Backoff
	}
	if agent.Spec.Restart.MaxRestarts == 0 {
		agent.Spec.Restart.MaxRestarts = defaults.Restart.MaxRestarts
	}
}

func applyDefaults(cfg *types.ClusterConfig) {
	// NATS defaults
	if cfg.Spec.NATS.Mode == "" {
		cfg.Spec.NATS.Mode = "embedded"
	}
	if cfg.Spec.NATS.Port == 0 {
		cfg.Spec.NATS.Port = 4222
	}
	if cfg.Spec.NATS.ClusterPort == 0 {
		cfg.Spec.NATS.ClusterPort = 6222
	}
	if cfg.Spec.NATS.JetStream.MaxMemory == "" {
		cfg.Spec.NATS.JetStream.MaxMemory = "1GB"
	}
	if cfg.Spec.NATS.JetStream.MaxStorage == "" {
		cfg.Spec.NATS.JetStream.MaxStorage = "10GB"
	}

	// Health defaults
	if cfg.Spec.Defaults.Health.Interval == 0 {
		cfg.Spec.Defaults.Health.Interval = 30 * time.Second
	}
	if cfg.Spec.Defaults.Health.Timeout == 0 {
		cfg.Spec.Defaults.Health.Timeout = 15 * time.Second
	}
	if cfg.Spec.Defaults.Health.MaxFailures == 0 {
		cfg.Spec.Defaults.Health.MaxFailures = 3
	}

	// Restart defaults
	if cfg.Spec.Defaults.Restart.Policy == "" {
		cfg.Spec.Defaults.Restart.Policy = restartOnFailure
	}
	if cfg.Spec.Defaults.Restart.Backoff == 0 {
		cfg.Spec.Defaults.Restart.Backoff = 10 * time.Second
	}
	if cfg.Spec.Defaults.Restart.MaxRestarts == 0 {
		cfg.Spec.Defaults.Restart.MaxRestarts = 5
	}
}
