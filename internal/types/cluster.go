package types

import "time"

// ClusterConfig represents the parsed cluster.yaml configuration.
type ClusterConfig struct {
	APIVersion string          `yaml:"apiVersion" json:"apiVersion"`
	Kind       string          `yaml:"kind" json:"kind"`
	Metadata   ClusterMetadata `yaml:"metadata" json:"metadata"`
	Spec       ClusterSpec     `yaml:"spec" json:"spec"`
}

type ClusterMetadata struct {
	Name string `yaml:"name" json:"name"`
}

type ClusterSpec struct {
	NATS          NATSConfig          `yaml:"nats" json:"nats"`
	Defaults      DefaultsConfig      `yaml:"defaults" json:"defaults"`
	MQTT          MQTTConfig          `yaml:"mqtt,omitempty" json:"mqtt,omitempty"`
	Dashboard     DashboardConfig     `yaml:"dashboard,omitempty" json:"dashboard,omitempty"`
	Metrics       MetricsConfig       `yaml:"metrics,omitempty" json:"metrics,omitempty"`
	Logging       LoggingConfig       `yaml:"logging,omitempty" json:"logging,omitempty"`
	Secrets       map[string]string   `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	Models        []ModelConfig       `yaml:"models,omitempty" json:"models,omitempty"`
	Nodes         NodeConfig          `yaml:"nodes,omitempty" json:"nodes,omitempty"`
	VM            VMConfig            `yaml:"vm,omitempty" json:"vm,omitempty"`
	Director      DirectorConfig      `yaml:"director,omitempty" json:"director,omitempty"`
	Users         []UserConfig        `yaml:"users,omitempty" json:"users,omitempty"`
	Communication CommunicationConfig `yaml:"communication,omitempty" json:"communication,omitempty"`
}

// ModelConfig defines a model entry in the cluster model registry.
type ModelConfig struct {
	Name     string `yaml:"name" json:"name"`
	Provider string `yaml:"provider" json:"provider"`
	Endpoint string `yaml:"endpoint,omitempty" json:"endpoint,omitempty"`
}

// NodeConfig defines cluster-level node settings.
type NodeConfig struct {
	AutoApprove bool `yaml:"autoApprove" json:"autoApprove"`
}

// VMConfig defines cluster-level VM settings.
type VMConfig struct {
	KernelPath string `yaml:"kernelPath,omitempty" json:"kernelPath,omitempty"`
	RootfsPath string `yaml:"rootfsPath,omitempty" json:"rootfsPath,omitempty"`
}

// DirectorConfig defines the top-level director agent reference.
type DirectorConfig struct {
	AgentID string `yaml:"agentId,omitempty" json:"agentId,omitempty"`
}

// UserConfig defines a user entry for multi-user mode.
type UserConfig struct {
	ID     string   `yaml:"id" json:"id"`
	Name   string   `yaml:"name,omitempty" json:"name,omitempty"`
	Role   string   `yaml:"role" json:"role"`
	Token  string   `yaml:"token,omitempty" json:"token,omitempty"`
	Teams  []string `yaml:"teams,omitempty" json:"teams,omitempty"`
	Agents []string `yaml:"agents,omitempty" json:"agents,omitempty"`
}

// CommunicationConfig defines cluster-level communication settings.
type CommunicationConfig struct {
	CrossTeam CrossTeamConfig `yaml:"crossTeamCapabilities,omitempty" json:"crossTeamCapabilities,omitempty"`
}

// CrossTeamConfig defines cross-team capability settings.
type CrossTeamConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

type MQTTConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	Port    int  `yaml:"port" json:"port"`
}

func (m MQTTConfig) EffectivePort() int {
	if m.Port == 0 {
		return 1883
	}
	return m.Port
}

type DashboardConfig struct {
	Enabled    bool   `yaml:"enabled" json:"enabled"`
	Addr       string `yaml:"addr" json:"addr"`
	CORSOrigin string `yaml:"corsOrigin,omitempty" json:"corsOrigin,omitempty"`
	AuthToken  string `yaml:"authToken,omitempty" json:"authToken,omitempty"`
}

func (d DashboardConfig) EffectiveAddr() string {
	if d.Addr == "" {
		return ":8080"
	}
	return d.Addr
}

type MetricsConfig struct {
	Enabled bool   `yaml:"enabled" json:"enabled"`
	Addr    string `yaml:"addr" json:"addr"`
}

func (m MetricsConfig) EffectiveAddr() string {
	if m.Addr == "" {
		return ":9090"
	}
	return m.Addr
}

type LoggingConfig struct {
	Enabled       bool `yaml:"enabled" json:"enabled"`
	RetentionDays int  `yaml:"retentionDays" json:"retentionDays"`
}

func (l LoggingConfig) EffectiveRetentionDays() int {
	if l.RetentionDays == 0 {
		return 30
	}
	return l.RetentionDays
}

type NATSConfig struct {
	Port         int             `yaml:"port" json:"port"`
	ClusterPort  int             `yaml:"clusterPort" json:"clusterPort"`
	JetStream    JetStreamConfig `yaml:"jetstream" json:"jetstream"`
	Mode         string          `yaml:"mode,omitempty" json:"mode,omitempty"`                 // "embedded" or "external"
	URLs         []string        `yaml:"urls,omitempty" json:"urls,omitempty"`                  // external NATS URLs
	ClusterPeers []string        `yaml:"clusterPeers,omitempty" json:"clusterPeers,omitempty"` // for NATS clustering
	// AuthToken is the NATS authentication token. If empty, a random token is
	// generated at startup and written to .state/nats-auth-token for clients.
	AuthToken string `yaml:"authToken,omitempty" json:"authToken,omitempty"`
	// Host is the bind address for the NATS server. Defaults to "127.0.0.1"
	// to prevent any external network access.
	Host string `yaml:"host,omitempty" json:"host,omitempty"`
}

type JetStreamConfig struct {
	Enabled    *bool  `yaml:"enabled" json:"enabled"`
	StorePath  string `yaml:"storePath" json:"storePath"`
	MaxMemory  string `yaml:"maxMemory" json:"maxMemory"`
	MaxStorage string `yaml:"maxStorage" json:"maxStorage"`
}

// IsEnabled returns whether JetStream is enabled (default true).
func (j JetStreamConfig) IsEnabled() bool {
	if j.Enabled == nil {
		return true
	}
	return *j.Enabled
}

type DefaultsConfig struct {
	Resources ResourceDefaults `yaml:"resources" json:"resources"`
	Health    HealthConfig     `yaml:"health" json:"health"`
	Restart   RestartConfig    `yaml:"restart" json:"restart"`
}

type ResourceDefaults struct {
	Memory string `yaml:"memory" json:"memory"`
	VCPUs  int    `yaml:"vcpus" json:"vcpus"`
	Disk   string `yaml:"disk" json:"disk"`
}

type HealthConfig struct {
	Enabled     *bool         `yaml:"enabled" json:"enabled"`
	Interval    time.Duration `yaml:"interval" json:"interval"`
	Timeout     time.Duration `yaml:"timeout" json:"timeout"`
	MaxFailures int           `yaml:"maxFailures" json:"maxFailures"`
}

// IsEnabled returns whether health checks are enabled (default true).
func (h HealthConfig) IsEnabled() bool {
	if h.Enabled == nil {
		return true
	}
	return *h.Enabled
}

type RestartConfig struct {
	Policy      string        `yaml:"policy" json:"policy"`
	MaxRestarts int           `yaml:"maxRestarts" json:"maxRestarts"`
	Backoff     time.Duration `yaml:"backoff" json:"backoff"`
}
