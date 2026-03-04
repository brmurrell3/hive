package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hivehq/hive/internal/types"
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

	// Apply spec
	cfg.Spec.NATS = types.NATSConfig{
		Port:        raw.Spec.NATS.Port,
		ClusterPort: raw.Spec.NATS.ClusterPort,
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
	healthTimeout, err := parseDurationOrDefault(raw.Spec.Defaults.Health.Timeout, 5*time.Second)
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
	NATS     rawNATSConfig     `yaml:"nats"`
	Defaults rawDefaultsConfig `yaml:"defaults"`
}

type rawNATSConfig struct {
	Port        int                `yaml:"port"`
	ClusterPort int                `yaml:"clusterPort"`
	JetStream   rawJetStreamConfig `yaml:"jetstream"`
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
		manifestPath := filepath.Join(agentsDir, entry.Name(), "manifest.yaml")
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("reading agent manifest %s: %w", entry.Name(), err)
		}

		agent, err := ParseAgent(data)
		if err != nil {
			return nil, fmt.Errorf("parsing agent manifest %s: %w", entry.Name(), err)
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
	agent.Spec.Capabilities = raw.Spec.Capabilities
	agent.Spec.Network = raw.Spec.Network
	agent.Spec.Volumes = raw.Spec.Volumes
	agent.Spec.Placement = raw.Spec.Placement
	agent.Spec.Firmware = raw.Spec.Firmware
	agent.Spec.Hardware = raw.Spec.Hardware

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
	Health       rawAgentHealth          `yaml:"health"`
	Restart      rawAgentRestart         `yaml:"restart"`
	Placement    types.AgentPlacement    `yaml:"placement"`
	Firmware     types.AgentFirmware     `yaml:"firmware"`
	Hardware     types.AgentHardware     `yaml:"hardware"`
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
		if filepath.Ext(entry.Name()) != ".yaml" {
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

		teams[team.Metadata.ID] = team
	}

	return teams, nil
}

// ParseTeam parses team manifest YAML content into a TeamManifest.
func ParseTeam(data []byte) (*types.TeamManifest, error) {
	var team types.TeamManifest
	if err := yaml.Unmarshal(data, &team); err != nil {
		return nil, fmt.Errorf("parsing team manifest: %w", err)
	}
	return &team, nil
}

// LoadDesiredState loads the complete desired state from a cluster root directory.
// It reads cluster.yaml, all agent manifests, and all team manifests.
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

	return &types.DesiredState{
		Cluster: cluster,
		Agents:  agents,
		Teams:   teams,
	}, nil
}

func applyDefaults(cfg *types.ClusterConfig) {
	// NATS defaults
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
	if cfg.Spec.Defaults.Health.MaxFailures == 0 {
		cfg.Spec.Defaults.Health.MaxFailures = 3
	}

	// Restart defaults
	if cfg.Spec.Defaults.Restart.Policy == "" {
		cfg.Spec.Defaults.Restart.Policy = "on-failure"
	}
	if cfg.Spec.Defaults.Restart.MaxRestarts == 0 {
		cfg.Spec.Defaults.Restart.MaxRestarts = 5
	}
}
