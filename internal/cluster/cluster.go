package cluster

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/hivehq/hive/internal/state"
)

// Role defines whether this node is the root (control plane) or a worker.
type Role string

const (
	RoleRoot   Role = "root"
	RoleWorker Role = "worker"
)

// Config holds the configuration for cluster membership.
type Config struct {
	Role        Role
	NATSMode    string   // "embedded" or "external"
	NATSUrls    []string // external NATS server URLs
	ClusterRoot string
}

// Cluster manages multi-node clustering, including NATS mode selection
// and state replication.
type Cluster struct {
	mu      sync.Mutex
	cfg     Config
	store   *state.Store
	logger  *slog.Logger
	running bool
	stopCh  chan struct{}
	stopped chan struct{}
}

// NewCluster creates a new Cluster with the given configuration.
func NewCluster(cfg Config, store *state.Store, logger *slog.Logger) *Cluster {
	return &Cluster{
		cfg:     cfg,
		store:   store,
		logger:  logger,
		stopCh:  make(chan struct{}),
		stopped: make(chan struct{}),
	}
}

// Start initializes the cluster subsystem. For root nodes this is a no-op
// beyond logging; for worker nodes it would connect to the external NATS
// cluster and begin state synchronization.
func (c *Cluster) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.running {
		return fmt.Errorf("cluster already running")
	}

	c.logger.Info("cluster starting",
		"role", c.cfg.Role,
		"nats_mode", c.cfg.NATSMode,
	)

	if c.cfg.NATSMode == "external" && len(c.cfg.NATSUrls) == 0 {
		return fmt.Errorf("external NATS mode requires at least one URL")
	}

	c.running = true

	go func() {
		defer close(c.stopped)
		<-c.stopCh
		c.logger.Info("cluster stopped")
	}()

	return nil
}

// Stop shuts down the cluster subsystem.
func (c *Cluster) Stop() {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return
	}
	c.running = false
	c.mu.Unlock()

	close(c.stopCh)
	<-c.stopped
}

// IsRoot returns true if this node is the root (control plane) node.
func (c *Cluster) IsRoot() bool {
	return c.cfg.Role == RoleRoot
}

// ReplicateState publishes state data to the given NATS subject for
// replication to other nodes. In embedded mode this is a no-op since
// all state is local. In external mode, this would publish to the
// shared NATS cluster.
func (c *Cluster) ReplicateState(subject string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.running {
		return fmt.Errorf("cluster not running")
	}

	if c.cfg.NATSMode == "embedded" {
		// In embedded mode, state replication is handled locally.
		c.logger.Debug("state replication skipped in embedded mode",
			"subject", subject,
		)
		return nil
	}

	// In external mode, we would publish to the NATS cluster.
	// This is a placeholder for the actual NATS publish implementation
	// which will be wired up when the NATS client is connected to
	// external servers.
	c.logger.Info("replicating state",
		"subject", subject,
		"bytes", len(data),
	)

	return nil
}

// NATSMode returns the configured NATS mode ("embedded" or "external").
func (c *Cluster) NATSMode() string {
	return c.cfg.NATSMode
}

// NATSUrls returns the configured external NATS URLs.
// Returns nil for embedded mode.
func (c *Cluster) NATSUrls() []string {
	if c.cfg.NATSMode == "embedded" {
		return nil
	}
	return c.cfg.NATSUrls
}
