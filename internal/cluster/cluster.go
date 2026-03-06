package cluster

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/nats-io/nats.go"
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
	nc      *nats.Conn
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

	if c.cfg.NATSMode == "external" {
		opts := []nats.Option{
			nats.Name("hive-cluster-" + string(c.cfg.Role)),
			nats.MaxReconnects(-1),
			nats.ReconnectWait(2 * time.Second),
			nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
				c.logger.Warn("NATS disconnected", "error", err)
			}),
			nats.ReconnectHandler(func(_ *nats.Conn) {
				c.logger.Info("NATS reconnected")
			}),
		}
		nc, err := nats.Connect(strings.Join(c.cfg.NATSUrls, ","), opts...)
		if err != nil {
			return fmt.Errorf("connecting to external NATS: %w", err)
		}
		c.nc = nc
		c.logger.Info("connected to external NATS",
			"urls", c.cfg.NATSUrls,
		)
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

	if c.nc != nil {
		if err := c.nc.Drain(); err != nil {
			c.logger.Warn("error draining NATS connection", "error", err)
		}
		c.nc = nil
	}

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

	// In external mode, publish to the NATS cluster.
	if c.nc == nil {
		return fmt.Errorf("NATS connection not established")
	}

	if err := c.nc.Publish(subject, data); err != nil {
		return fmt.Errorf("publishing state replication: %w", err)
	}

	c.logger.Debug("replicated state",
		"subject", subject,
		"bytes", len(data),
	)

	return nil
}

// Subscribe subscribes to a NATS subject for receiving state replication
// updates from other nodes. This is used by worker nodes to receive state
// published by the root node. Returns an error if not in external mode
// or if the cluster is not running.
func (c *Cluster) Subscribe(subject string, handler func(data []byte)) (*nats.Subscription, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.running {
		return nil, fmt.Errorf("cluster not running")
	}

	if c.cfg.NATSMode == "embedded" {
		return nil, fmt.Errorf("subscribe not supported in embedded mode")
	}

	if c.nc == nil {
		return nil, fmt.Errorf("NATS connection not established")
	}

	sub, err := c.nc.Subscribe(subject, func(msg *nats.Msg) {
		handler(msg.Data)
	})
	if err != nil {
		return nil, fmt.Errorf("subscribing to %s: %w", subject, err)
	}

	c.logger.Info("subscribed to state replication",
		"subject", subject,
	)

	return sub, nil
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
