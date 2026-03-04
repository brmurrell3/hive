// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package nats wraps an embedded NATS server with TLS, JetStream, and hardened security defaults.
package nats

import (
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/config"
	"github.com/brmurrell3/hive/internal/types"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

// Server wraps an embedded NATS server.
type Server struct {
	mu        sync.Mutex
	ns        *natsserver.Server
	config    types.NATSConfig
	authToken string // resolved token (either from config or auto-generated)
	logger    *slog.Logger
	started   bool
	starting  bool
}

// NewServer creates a new embedded NATS server from the cluster config.
// If cfg.AuthToken is empty, a cryptographically random 32-byte hex token is
// generated and stored so callers can retrieve it via AuthToken().
// If cfg.Host is empty, the server binds to "127.0.0.1" for security.
func NewServer(cfg types.NATSConfig, dataDir string, logger *slog.Logger) (*Server, error) {
	// Validate port range. Port -1 is allowed (random port assignment by the
	// NATS server), 0 is the NATS default, and 1-65535 are valid TCP ports.
	if cfg.Port < -1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("invalid NATS port %d: must be -1 (random) or 0-65535", cfg.Port)
	}

	// Resolve bind host (default to localhost for security).
	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}

	if host == "0.0.0.0" || host == "::" {
		logger.Warn("NATS server binding to all interfaces", "host", host)
	}

	// Resolve auth token — generate one if not configured.
	authToken := cfg.AuthToken
	if authToken == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, fmt.Errorf("generating NATS auth token: %w", err)
		}
		authToken = hex.EncodeToString(raw)
		logger.Info("no NATS auth token configured, generated random token")
	}

	// Apply connection limits (defaults if not configured).
	maxConn := cfg.MaxConnections
	if maxConn == 0 {
		maxConn = 1024
	}
	maxSubs := cfg.MaxSubscriptions
	if maxSubs == 0 {
		maxSubs = 10000
	}

	opts := &natsserver.Options{
		Host:          host,
		Port:          cfg.Port,
		Authorization: authToken,
		NoLog:         true,
		NoSigs:        true,
		MaxPayload:    2 * 1024 * 1024, // 2MB per spec
		MaxConn:       maxConn,
		MaxSubs:       maxSubs,
	}

	// Configure TLS for client connections if enabled.
	if cfg.TLS.Enabled {
		tlsCfg, err := loadTLSConfig(cfg.TLS.CertFile, cfg.TLS.KeyFile, cfg.TLS.CAFile)
		if err != nil {
			return nil, fmt.Errorf("loading NATS TLS config: %w", err)
		}
		opts.TLSConfig = tlsCfg
		opts.TLS = true
		opts.TLSTimeout = 5 // seconds
		logger.Info("NATS TLS enabled",
			"cert_file", cfg.TLS.CertFile,
			"ca_file", cfg.TLS.CAFile,
		)
	}

	if cfg.JetStream.IsEnabled() {
		opts.JetStream = true
		if dataDir != "" {
			opts.StoreDir = dataDir
		}
		if cfg.JetStream.StorePath != "" {
			opts.StoreDir = cfg.JetStream.StorePath
		}
		// Apply JetStream memory and storage limits if configured.
		if cfg.JetStream.MaxMemory != "" {
			maxMem, err := config.ParseMemory(cfg.JetStream.MaxMemory)
			if err != nil {
				return nil, fmt.Errorf("parsing jetstream maxMemory %q: %w", cfg.JetStream.MaxMemory, err)
			}
			opts.JetStreamMaxMemory = maxMem
			logger.Info("JetStream memory limit set", "max_memory_bytes", maxMem)
		}
		if cfg.JetStream.MaxStorage != "" {
			maxStore, err := config.ParseMemory(cfg.JetStream.MaxStorage)
			if err != nil {
				return nil, fmt.Errorf("parsing jetstream maxStorage %q: %w", cfg.JetStream.MaxStorage, err)
			}
			opts.JetStreamMaxStore = maxStore
			logger.Info("JetStream storage limit set", "max_storage_bytes", maxStore)
		}
	}

	// Configure NATS clustering if a cluster port is specified.
	if cfg.ClusterPort > 0 {
		clusterName := cfg.ClusterName
		if clusterName == "" {
			clusterName = "hive-cluster"
		}
		opts.Cluster.Name = clusterName
		opts.Cluster.Port = cfg.ClusterPort
		opts.Cluster.Host = host

		// Require cluster route authentication when clustering is enabled.
		// NATS cluster routes use username/password; we set the token as
		// the password with a fixed username for simplicity.
		if cfg.ClusterAuthToken == "" {
			return nil, fmt.Errorf("ClusterAuthToken is required when ClusterPort is set (port %d) — refusing to start with unauthenticated cluster routes", cfg.ClusterPort)
		}
		opts.Cluster.Username = clusterName
		opts.Cluster.Password = cfg.ClusterAuthToken

		// Apply TLS to cluster routes if enabled.
		if cfg.TLS.Enabled {
			clusterTLS, err := loadTLSConfig(cfg.TLS.CertFile, cfg.TLS.KeyFile, cfg.TLS.CAFile)
			if err != nil {
				return nil, fmt.Errorf("loading NATS cluster TLS config: %w", err)
			}
			opts.Cluster.TLSConfig = clusterTLS
			opts.Cluster.TLSTimeout = 5
		}

		logger.Info("NATS clustering enabled",
			"cluster_port", cfg.ClusterPort,
			"cluster_name", clusterName,
		)
	}

	// Warn if cluster peers are configured but no cluster port is set.
	if len(cfg.ClusterPeers) > 0 && cfg.ClusterPort == 0 {
		logger.Warn("cluster peers configured but no cluster port is set; peers will be ignored",
			"peers", cfg.ClusterPeers,
		)
	}

	// Parse and validate cluster peer URLs. Only attach them as NATS routes
	// when a cluster port is configured; otherwise peers have already been
	// warned about and are not attached to the server options.
	if len(cfg.ClusterPeers) > 0 {
		routes := make([]*url.URL, 0, len(cfg.ClusterPeers))
		for _, peer := range cfg.ClusterPeers {
			u, err := url.Parse(peer)
			if err != nil {
				return nil, fmt.Errorf("parsing cluster peer URL %q: %w", peer, err)
			}
			routes = append(routes, u)
		}
		if cfg.ClusterPort > 0 {
			opts.Routes = routes
			logger.Info("NATS cluster routes configured",
				"peers", cfg.ClusterPeers,
			)
		}
	}

	ns, err := natsserver.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("creating NATS server: %w", err)
	}

	logger.Info("NATS server configured",
		"host", host,
		"port", cfg.Port,
		"auth_enabled", true,
	)

	return &Server{
		ns:        ns,
		config:    cfg,
		authToken: authToken,
		logger:    logger,
	}, nil
}

// Start starts the embedded NATS server and waits for it to be ready.
// The readiness timeout is taken from the config (ReadyTimeout); defaults
// to 10s if not set.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.started || s.starting {
		s.mu.Unlock()
		return fmt.Errorf("NATS server already started")
	}
	s.starting = true
	s.mu.Unlock()

	s.ns.Start()

	readyTimeout := s.config.ReadyTimeout
	if readyTimeout == 0 {
		readyTimeout = 10 * time.Second
	}

	if !s.ns.ReadyForConnections(readyTimeout) {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
		return fmt.Errorf("NATS server failed to become ready within %s", readyTimeout)
	}

	s.mu.Lock()
	s.started = true
	s.starting = false
	s.mu.Unlock()

	s.logger.Info("NATS server started",
		"port", s.config.Port,
		"jetstream", s.config.JetStream.IsEnabled(),
	)

	return nil
}

// Shutdown gracefully shuts down the NATS server. It waits up to
// ShutdownTimeout (default 30s) for the server to finish shutting down
// before returning with a warning. If the timeout fires, the goroutine
// waiting on WaitForShutdown continues in the background but is harmless
// since the process is already shutting down.
//
// Shutdown is a no-op if the server was never started successfully.
func (s *Server) Shutdown() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		s.logger.Debug("NATS server shutdown called but server was never started, skipping")
		return
	}
	s.started = false
	s.mu.Unlock()

	s.logger.Info("shutting down NATS server")
	s.ns.Shutdown()

	shutdownTimeout := s.config.ShutdownTimeout
	if shutdownTimeout == 0 {
		shutdownTimeout = 30 * time.Second
	}

	done := make(chan struct{})
	// The goroutine below may outlive Shutdown() if WaitForShutdown blocks
	// beyond the timeout. This is acceptable because Shutdown is only called
	// during process exit, so the leaked goroutine is cleaned up by the OS.
	go func() {
		s.ns.WaitForShutdown()
		close(done)
	}()

	select {
	case <-done:
		s.logger.Info("NATS server shutdown complete")
	case <-time.After(shutdownTimeout):
		s.logger.Warn("NATS server shutdown timed out, proceeding anyway",
			"timeout", shutdownTimeout,
		)
	}
}

// ClientURL returns the URL for NATS clients to connect.
func (s *Server) ClientURL() string {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if !started {
		return ""
	}
	return s.ns.ClientURL()
}

// AuthToken returns the NATS authentication token that clients must use.
// This is either the value from the cluster config or the auto-generated token.
func (s *Server) AuthToken() string {
	return s.authToken
}

// Port returns the actual port the server is listening on, parsed from the
// server's bound address. This handles the case where Port was configured as 0
// (random port assignment).
func (s *Server) Port() int {
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if !started {
		return 0
	}
	addr := s.ns.Addr()
	if addr == nil {
		return 0
	}
	tcpAddr, ok := addr.(*net.TCPAddr)
	if !ok {
		return 0
	}
	return tcpAddr.Port
}

// loadTLSConfig creates a tls.Config from the given certificate, key, and
// optional CA file paths. When a CA file is provided, client certificate
// verification (mTLS) is enabled.
//
// The ServerName is set from os.Hostname and is only used for the embedded
// server's listener TLS. For cluster route connections, the ServerName is
// overridden by the connecting side with the appropriate peer hostname.
func loadTLSConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("loading cert/key pair: %w", err)
	}

	// Resolve the server's hostname for SNI; fall back to "localhost" if
	// the system hostname cannot be determined. This ServerName is only used
	// for the embedded server's listener TLS and is overridden for cluster
	// route connections by the connecting peer.
	serverName, err := os.Hostname()
	if err != nil || serverName == "" {
		serverName = "localhost"
	}

	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		ServerName:   serverName,
		// Explicit hardened cipher suites for TLS 1.2 connections.
		// TLS 1.3 cipher suites are managed automatically by Go.
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
		tlsConfig.ClientCAs = pool
		tlsConfig.ClientAuth = tls.RequireAndVerifyClientCert
	}

	return tlsConfig, nil
}
