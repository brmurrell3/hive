// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package types

import (
	"strings"
	"time"
)

// TLSConfig holds TLS certificate configuration.
type TLSConfig struct {
	Enabled  bool   `yaml:"enabled" json:"enabled"`
	CertFile string `yaml:"certFile" json:"certFile"`
	KeyFile  string `yaml:"keyFile" json:"keyFile"`
	CAFile   string `yaml:"caFile,omitempty" json:"caFile,omitempty"`
}

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
	NATS        NATSConfig        `yaml:"nats" json:"nats"`
	Defaults    DefaultsConfig    `yaml:"defaults" json:"defaults"`
	Dashboard   DashboardConfig   `yaml:"dashboard,omitempty" json:"dashboard,omitempty"`
	Metrics     MetricsConfig     `yaml:"metrics,omitempty" json:"metrics,omitempty"`
	Logging     LoggingConfig     `yaml:"logging,omitempty" json:"logging,omitempty"`
	Secrets     map[string]string `yaml:"secrets,omitempty" json:"secrets,omitempty"`
	SecretsFile string            `yaml:"secretsFile,omitempty" json:"secretsFile,omitempty"`
	Models      []ModelConfig     `yaml:"models,omitempty" json:"models,omitempty"`
	Nodes       NodeConfig        `yaml:"nodes,omitempty" json:"nodes,omitempty"`
	VM          VMConfig          `yaml:"vm,omitempty" json:"vm,omitempty"`
	Users       []UserConfig      `yaml:"users,omitempty" json:"users,omitempty"`
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
	KernelPath    string `yaml:"kernelPath,omitempty" json:"kernelPath,omitempty"`
	RootfsPath    string `yaml:"rootfsPath,omitempty" json:"rootfsPath,omitempty"`
	TotalMemoryMB int64  `yaml:"totalMemoryMB,omitempty" json:"totalMemoryMB,omitempty"` // Total memory available for VMs (0 = unlimited)
	TotalVCPUs    int64  `yaml:"totalVCPUs,omitempty" json:"totalVCPUs,omitempty"`       // Total vCPUs available for VMs (0 = unlimited)
	// ImageURL overrides the default GitHub Releases URL for downloading kernel
	// and rootfs images. This supports air-gapped environments where images are
	// hosted on an internal mirror or local file server. Must use https:// or
	// file:// scheme.
	ImageURL string `yaml:"imageURL,omitempty" json:"imageURL,omitempty"`
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

type DashboardConfig struct {
	Enabled    bool      `yaml:"enabled" json:"enabled"`
	Addr       string    `yaml:"addr" json:"addr"`
	CORSOrigin string    `yaml:"corsOrigin,omitempty" json:"corsOrigin,omitempty"`
	AuthToken  string    `yaml:"authToken,omitempty" json:"authToken,omitempty"`
	TLS        TLSConfig `yaml:"tls,omitempty" json:"tls,omitempty"`
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
	URLs         []string        `yaml:"urls,omitempty" json:"urls,omitempty"`                 // external NATS URLs
	ClusterPeers []string        `yaml:"clusterPeers,omitempty" json:"clusterPeers,omitempty"` // for NATS clustering
	// AuthToken is the NATS authentication token. If empty, a random token is
	// generated at startup and written to .state/nats-auth-token for clients.
	AuthToken string `yaml:"authToken,omitempty" json:"authToken,omitempty"`
	// ClusterAuthToken is used for authenticating inter-node cluster route
	// connections. If empty, cluster routes have no authentication.
	ClusterAuthToken string `yaml:"clusterAuthToken,omitempty" json:"clusterAuthToken,omitempty"`
	// Host is the bind address for the NATS server. Defaults to "127.0.0.1"
	// to prevent any external network access.
	Host string `yaml:"host,omitempty" json:"host,omitempty"`
	// TLS holds TLS configuration for NATS client and cluster connections.
	TLS TLSConfig `yaml:"tls,omitempty" json:"tls,omitempty"`
	// MaxConnections is the maximum number of concurrent client connections.
	// Defaults to 1024 if zero.
	MaxConnections int `yaml:"maxConnections,omitempty" json:"maxConnections,omitempty"`
	// MaxSubscriptions is the maximum number of subscriptions per connection.
	// Defaults to 10000 if zero.
	MaxSubscriptions int `yaml:"maxSubscriptions,omitempty" json:"maxSubscriptions,omitempty"`
	// ClusterName is the NATS cluster name. Defaults to "hive-cluster".
	ClusterName string `yaml:"clusterName,omitempty" json:"clusterName,omitempty"`
	// ReadyTimeout is how long to wait for the NATS server to become ready.
	// Defaults to 10s if zero.
	ReadyTimeout time.Duration `yaml:"readyTimeout,omitempty" json:"readyTimeout,omitempty"`
	// ShutdownTimeout is how long to wait for the NATS server to finish
	// shutting down before logging a warning and proceeding. Defaults to 30s
	// if zero. The background goroutine waiting on WaitForShutdown will
	// eventually complete when the NATS server finishes; it is harmless
	// since the process is already shutting down.
	ShutdownTimeout time.Duration `yaml:"shutdownTimeout,omitempty" json:"shutdownTimeout,omitempty"`
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

const redactedPlaceholder = "***REDACTED***"

// redactString replaces a non-empty string with a redacted placeholder.
func redactString(s string) string {
	if s == "" {
		return ""
	}
	return redactedPlaceholder
}

// Redacted returns a deep copy of the ClusterConfig with sensitive fields
// masked. Use this for logging or diagnostics -- never log the raw config
// directly as it may contain secrets and tokens in plaintext.
func (c ClusterConfig) Redacted() ClusterConfig {
	r := c

	// Deep-copy all slice fields to avoid sharing backing arrays with
	// the original config.
	if r.Spec.NATS.URLs != nil {
		r.Spec.NATS.URLs = append([]string(nil), r.Spec.NATS.URLs...)
	}
	if r.Spec.NATS.ClusterPeers != nil {
		r.Spec.NATS.ClusterPeers = append([]string(nil), r.Spec.NATS.ClusterPeers...)
	}
	if r.Spec.Models != nil {
		r.Spec.Models = append([]ModelConfig(nil), r.Spec.Models...)
	}
	// Redact NATS auth tokens.
	r.Spec.NATS.AuthToken = redactString(r.Spec.NATS.AuthToken)
	r.Spec.NATS.ClusterAuthToken = redactString(r.Spec.NATS.ClusterAuthToken)

	// Redact dashboard auth token.
	r.Spec.Dashboard.AuthToken = redactString(r.Spec.Dashboard.AuthToken)

	// Redact secrets map (deep copy with redacted values).
	if len(r.Spec.Secrets) > 0 {
		redacted := make(map[string]string, len(r.Spec.Secrets))
		for k := range r.Spec.Secrets {
			redacted[k] = redactedPlaceholder
		}
		r.Spec.Secrets = redacted
	}

	// Redact secrets file path. The entire path is fully redacted for security
	// since even the path itself may reveal infrastructure details.
	r.Spec.SecretsFile = redactString(r.Spec.SecretsFile)

	// Redact user tokens (deep copy the slice).
	if len(r.Spec.Users) > 0 {
		users := make([]UserConfig, len(r.Spec.Users))
		copy(users, r.Spec.Users)
		for i := range users {
			users[i].Token = redactString(users[i].Token)
			// Deep-copy per-user slices.
			if users[i].Teams != nil {
				users[i].Teams = append([]string(nil), users[i].Teams...)
			}
			if users[i].Agents != nil {
				users[i].Agents = append([]string(nil), users[i].Agents...)
			}
		}
		r.Spec.Users = users
	}

	// Redact TLS key file paths (they point to private keys).
	r.Spec.NATS.TLS.KeyFile = redactKeyPath(r.Spec.NATS.TLS.KeyFile)
	r.Spec.Dashboard.TLS.KeyFile = redactKeyPath(r.Spec.Dashboard.TLS.KeyFile)

	return r
}

// redactKeyPath redacts the directory portion of a key file path, preserving
// only the filename for diagnostics.
func redactKeyPath(path string) string {
	if path == "" {
		return ""
	}
	idx := strings.LastIndex(path, "/")
	if idx >= 0 {
		return redactedPlaceholder + "/" + path[idx+1:]
	}
	return path
}
