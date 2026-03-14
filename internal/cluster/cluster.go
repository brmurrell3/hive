// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package cluster manages multi-node clustering over NATS with TLS, state replication, and reconnect logic.
// TODO: Add unit tests for cluster lifecycle and state replication.
package cluster

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
)

const (
	// natsReconnectWait is the initial delay between NATS reconnect attempts.
	natsReconnectWait = 2 * time.Second

	// natsConnectTimeout is the timeout for the initial NATS connection.
	natsConnectTimeout = 10 * time.Second

	// reconnectBaseDelay is the base duration for exponential reconnect backoff.
	reconnectBaseDelay = 500 * time.Millisecond

	// reconnectMaxBackoff is the maximum duration for reconnect backoff.
	reconnectMaxBackoff = 30 * time.Second

	// flushTimeout is the timeout for flushing pending NATS messages during state replication.
	flushTimeout = 5 * time.Second

	// drainTimeout is how long to wait for NATS drain to complete during shutdown.
	drainTimeout = 10 * time.Second
)

// Role defines whether this node is the root (control plane) or a worker.
type Role string

const (
	RoleRoot   Role = "root"
	RoleWorker Role = "worker"
)

const (
	NATSModeEmbedded = "embedded"
	NATSModeExternal = "external"
)

// Config holds the configuration for cluster membership.
// NOTE: ClusterRoot was previously defined here but was never read within
// the cluster package. It has been removed. The filesystem root used by
// firmware builds is tracked in the firmware package's own BuildConfig.
type Config struct {
	Role     Role
	NATSMode string   // NATSModeEmbedded or NATSModeExternal
	NATSUrls []string // external NATS server URLs
	// TLS holds optional TLS configuration for external NATS connections.
	TLS *types.TLSConfig
	// AuthToken is the NATS authentication token for external connections.
	AuthToken string
}

// Cluster manages multi-node clustering, including NATS mode selection
// and state replication. A Cluster instance is single-use: once Stop is
// called, the instance cannot be restarted. Create a new Cluster for
// subsequent use.
type Cluster struct {
	mu              sync.Mutex
	cfg             Config
	store           *state.Store
	nc              *nats.Conn
	subs            []*nats.Subscription // tracked subscriptions for cleanup
	logger          *slog.Logger
	running         bool
	monitorLaunched bool // true if the background goroutine was started
	stopOnce        sync.Once
	stoppedOnce     sync.Once // guards closing the stopped channel
	stopCh          chan struct{}
	stopped         chan struct{}
}

// NewCluster creates a new Cluster with the given configuration.
// The cfg.NATSUrls slice is deep-copied to prevent the caller from mutating
// the Cluster's internal state after construction.
func NewCluster(cfg Config, store *state.Store, logger *slog.Logger) *Cluster {
	// Deep-copy NATSUrls to avoid the caller mutating the slice after
	// passing it in (M2: stored by reference).
	if cfg.NATSUrls != nil {
		urls := make([]string, len(cfg.NATSUrls))
		copy(urls, cfg.NATSUrls)
		cfg.NATSUrls = urls
	}

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

	// Validate Role is a known value.
	if c.cfg.Role != RoleRoot && c.cfg.Role != RoleWorker {
		return fmt.Errorf("invalid Role %q: must be %q or %q", c.cfg.Role, RoleRoot, RoleWorker)
	}

	// Validate NATSMode is a known value.
	if c.cfg.NATSMode != NATSModeEmbedded && c.cfg.NATSMode != NATSModeExternal {
		return fmt.Errorf("invalid NATSMode %q: must be \"embedded\" or \"external\"", c.cfg.NATSMode)
	}

	if c.cfg.NATSMode == NATSModeExternal && len(c.cfg.NATSUrls) == 0 {
		return fmt.Errorf("external NATS mode requires at least one URL")
	}

	if c.cfg.NATSMode == NATSModeExternal {
		opts := []nats.Option{
			nats.Name("hive-cluster-" + string(c.cfg.Role)),
			nats.MaxReconnects(-1),
			nats.ReconnectWait(natsReconnectWait),
			nats.Timeout(natsConnectTimeout),
			nats.CustomReconnectDelay(func(attempts int) time.Duration {
				// Exponential backoff with jitter: base * 2^attempts, capped at 30s.
				if attempts > 20 {
					attempts = 20
				}
				base := float64(reconnectBaseDelay)
				backoff := base * math.Pow(2, float64(attempts))
				maxBackoff := float64(reconnectMaxBackoff)
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				// Add jitter: +/- 25%
				jitter := backoff * 0.25 * (2*rand.Float64() - 1)
				return time.Duration(backoff + jitter)
			}),
			nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
				c.logger.Warn("NATS disconnected", "error", err)
			}),
			nats.ReconnectHandler(func(_ *nats.Conn) {
				c.logger.Info("NATS reconnected")
			}),
			nats.ClosedHandler(func(nc *nats.Conn) {
				c.logger.Error("NATS connection permanently closed", "error", nc.LastError())
			}),
		}

		// Add auth token if configured.
		if c.cfg.AuthToken != "" {
			opts = append(opts, nats.Token(c.cfg.AuthToken))
		}

		// Add TLS if configured.
		if c.cfg.TLS != nil && c.cfg.TLS.Enabled {
			// Extract the hostname from the first NATS URL for TLS SNI verification.
			// Limitation (M9): Only the first URL's hostname is used for the TLS
			// ServerName (SNI). In multi-host clusters where each NATS server has a
			// distinct hostname, the TLS handshake will verify against this single
			// name. The Go NATS client reuses the same tls.Config for all servers in
			// the pool, so per-route ServerName would require a custom TLS dialer or
			// VerifyPeerCertificate hook — which is not currently supported by the
			// nats.go library's Secure() option. For clusters where all NATS nodes
			// share a wildcard or SAN certificate this is not an issue.
			serverName := extractHostFromURL(c.cfg.NATSUrls[0])
			tlsConfig, err := loadClusterTLSConfig(c.cfg.TLS.CertFile, c.cfg.TLS.KeyFile, c.cfg.TLS.CAFile, serverName)
			if err != nil {
				return fmt.Errorf("loading cluster TLS config: %w", err)
			}
			opts = append(opts, nats.Secure(tlsConfig))
			c.logger.Info("cluster NATS TLS enabled")
		}

		for _, rawURL := range c.cfg.NATSUrls {
			u, err := url.Parse(rawURL)
			if err != nil || (u.Scheme != "nats" && u.Scheme != "tls" && u.Scheme != "nats+tls") {
				c.logger.Warn("NATS URL has invalid or unsupported scheme", "url", rawURL)
			}
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
	c.monitorLaunched = true // set under Start's lock, before goroutine launch

	go func() {
		defer c.stoppedOnce.Do(func() { close(c.stopped) })
		<-c.stopCh
		c.logger.Info("cluster stopped")
	}()

	return nil
}

// Stop shuts down the cluster subsystem.
func (c *Cluster) Stop() {
	c.mu.Lock()
	if !c.running {
		monitorWasLaunched := c.monitorLaunched
		c.mu.Unlock()
		// Even if Start() was never called or already stopped, ensure
		// stopOnce fires so that any future <-c.stopped won't block.
		c.stopOnce.Do(func() {
			close(c.stopCh)
		})
		// If the monitoring goroutine was never launched, close stopped
		// ourselves to prevent deadlock. stoppedOnce ensures this is safe
		// even if the goroutine is racing to close it.
		if !monitorWasLaunched {
			c.stoppedOnce.Do(func() { close(c.stopped) })
		}
		return
	}
	c.running = false
	monitorWasLaunched := c.monitorLaunched

	// Unsubscribe all tracked subscriptions while holding the lock.
	for _, sub := range c.subs {
		if err := sub.Unsubscribe(); err != nil {
			c.logger.Warn("error unsubscribing", "subject", sub.Subject, "error", err)
		}
	}
	c.subs = nil

	// Grab the connection reference and nil it under the lock to prevent
	// concurrent use in ReplicateState.
	nc := c.nc
	c.nc = nil
	c.mu.Unlock()

	// Drain the connection outside the lock (it may block).
	// Drain() already flushes pending messages and waits for processing,
	// so a separate FlushTimeout is redundant. Wrap in a timeout to prevent
	// Stop() from blocking indefinitely if the remote is unresponsive.
	if nc != nil {
		drainDone := make(chan struct{})
		go func() {
			if err := nc.Drain(); err != nil {
				c.logger.Warn("error draining NATS connection", "error", err)
			}
			close(drainDone)
		}()
		select {
		case <-drainDone:
		case <-time.After(drainTimeout):
			c.logger.Warn("NATS drain timed out, forcing close")
			nc.Close()
		}
	}

	c.stopOnce.Do(func() { close(c.stopCh) })

	// Only wait for the monitoring goroutine if it was actually launched.
	if monitorWasLaunched {
		<-c.stopped
	}
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
	if err := types.ValidateSubjectField("subject", subject); err != nil {
		return fmt.Errorf("invalid replication subject: %w", err)
	}

	c.mu.Lock()

	if !c.running {
		c.mu.Unlock()
		return fmt.Errorf("cluster not running")
	}

	if c.cfg.NATSMode == NATSModeEmbedded {
		// In embedded mode, state replication is handled locally.
		c.mu.Unlock()
		c.logger.Debug("state replication skipped in embedded mode",
			"subject", subject,
		)
		return nil
	}

	// Snapshot the NATS connection reference under the lock so we can
	// publish outside the lock without holding it during I/O.
	nc := c.nc
	c.mu.Unlock()

	if nc == nil {
		return fmt.Errorf("cluster connection not available")
	}
	if nc.IsClosed() {
		return fmt.Errorf("cluster connection is closed")
	}

	if len(data) > 2*1024*1024 {
		return fmt.Errorf("state data exceeds maximum size: %d bytes", len(data))
	}

	if err := nc.Publish(subject, data); err != nil {
		return fmt.Errorf("publishing state replication: %w", err)
	}

	if err := nc.FlushTimeout(flushTimeout); err != nil {
		return fmt.Errorf("flushing state replication: %w", err)
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
//
// Subscriptions are tracked and automatically unsubscribed on Stop.
func (c *Cluster) Subscribe(subject string, handler func(data []byte)) (*nats.Subscription, error) {
	if err := types.ValidateSubjectField("subject", subject); err != nil {
		return nil, fmt.Errorf("invalid subscription subject: %w", err)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.running {
		return nil, fmt.Errorf("cluster not running")
	}

	if c.cfg.NATSMode == NATSModeEmbedded {
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

	// Track the subscription for cleanup on Stop.
	c.subs = append(c.subs, sub)

	c.logger.Info("subscribed to state replication",
		"subject", subject,
	)

	return sub, nil
}

// NATSMode returns the configured NATS mode (NATSModeEmbedded or NATSModeExternal).
func (c *Cluster) NATSMode() string {
	return c.cfg.NATSMode
}

// NATSUrls returns a copy of the configured external NATS URLs.
// Returns nil for embedded mode.
func (c *Cluster) NATSUrls() []string {
	if c.cfg.NATSMode == NATSModeEmbedded {
		return nil
	}
	cp := make([]string, len(c.cfg.NATSUrls))
	copy(cp, c.cfg.NATSUrls)
	return cp
}

// ReplicateAgentState serializes the given agent data and publishes it on the
// cluster state replication subject. This should be called after critical agent
// state transitions (start, stop, destroy, scale) so that worker nodes in
// external NATS mode receive the updated state.
func (c *Cluster) ReplicateAgentState(agentID string, agentData []byte) error {
	subject := protocol.SubjClusterState
	if err := c.ReplicateState(subject, agentData); err != nil {
		c.logger.Warn("failed to replicate agent state",
			"agent_id", agentID,
			"error", err,
		)
		return fmt.Errorf("replicating agent state for %s: %w", agentID, err)
	}
	return nil
}

// SubscribeStateUpdates subscribes to agent state replication updates from the
// leader node. When updates arrive, the handler is called with the raw agent
// state data. This is intended for worker nodes in external NATS mode.
// In embedded mode this is a no-op and returns nil.
func (c *Cluster) SubscribeStateUpdates(handler func(data []byte)) error {
	c.mu.Lock()
	natsMode := c.cfg.NATSMode
	c.mu.Unlock()

	if natsMode == NATSModeEmbedded {
		c.logger.Debug("state update subscription skipped in embedded mode")
		return nil
	}

	_, err := c.Subscribe(protocol.SubjClusterState, handler)
	if err != nil {
		return fmt.Errorf("subscribing to state updates: %w", err)
	}
	c.logger.Info("subscribed to cluster state updates")
	return nil
}

// extractHostFromURL parses a URL (e.g. "nats://host:4222") and returns just
// the hostname portion for use as a TLS ServerName. If parsing fails, the raw
// input is returned as-is.
func extractHostFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	host := u.Hostname()
	if host == "" {
		return rawURL
	}
	return host
}

// loadClusterTLSConfig creates a tls.Config from the given certificate, key,
// and optional CA file paths for use with external NATS connections.
// serverName sets the TLS ServerName (SNI) for certificate verification.
func loadClusterTLSConfig(certFile, keyFile, caFile, serverName string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading cert/key pair: %w", err)
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		ServerName:   serverName,
		CipherSuites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		},
	}

	if caFile != "" {
		caCert, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("reading CA file: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(caCert) {
			return nil, fmt.Errorf("failed to parse CA certificate")
		}
		tlsConfig.RootCAs = pool
	}

	return tlsConfig, nil
}
