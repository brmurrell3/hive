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
	NATS     NATSConfig     `yaml:"nats" json:"nats"`
	Defaults DefaultsConfig `yaml:"defaults" json:"defaults"`
}

type NATSConfig struct {
	Port         int             `yaml:"port" json:"port"`
	ClusterPort  int             `yaml:"clusterPort" json:"clusterPort"`
	JetStream    JetStreamConfig `yaml:"jetstream" json:"jetstream"`
	Mode         string          `yaml:"mode,omitempty" json:"mode,omitempty"`                 // "embedded" or "external"
	URLs         []string        `yaml:"urls,omitempty" json:"urls,omitempty"`                  // external NATS URLs
	ClusterPeers []string        `yaml:"clusterPeers,omitempty" json:"clusterPeers,omitempty"` // for NATS clustering
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
