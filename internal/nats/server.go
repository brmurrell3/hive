package nats

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"time"

	"github.com/hivehq/hive/internal/types"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

// Server wraps an embedded NATS server.
type Server struct {
	ns        *natsserver.Server
	config    types.NATSConfig
	authToken string // resolved token (either from config or auto-generated)
	logger    *slog.Logger
}

// NewServer creates a new embedded NATS server from the cluster config.
// If cfg.AuthToken is empty, a cryptographically random 32-byte hex token is
// generated and stored so callers can retrieve it via AuthToken().
// If cfg.Host is empty, the server binds to "127.0.0.1" for security.
func NewServer(cfg types.NATSConfig, dataDir string, logger *slog.Logger) (*Server, error) {
	// Resolve bind host (default to localhost for security).
	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
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

	opts := &natsserver.Options{
		Host:          host,
		Port:          cfg.Port,
		Authorization: authToken,
		NoLog:         true,
		NoSigs:        true,
		MaxPayload:    2 * 1024 * 1024, // 2MB per spec
	}

	if cfg.JetStream.IsEnabled() {
		opts.JetStream = true
		if dataDir != "" {
			opts.StoreDir = dataDir
		}
		if cfg.JetStream.StorePath != "" {
			opts.StoreDir = cfg.JetStream.StorePath
		}
	}

	// Configure NATS clustering if a cluster port is specified.
	if cfg.ClusterPort > 0 {
		opts.Cluster.Name = "hive-cluster"
		opts.Cluster.Port = cfg.ClusterPort
		opts.Cluster.Host = host
		logger.Info("NATS clustering enabled",
			"cluster_port", cfg.ClusterPort,
			"cluster_name", "hive-cluster",
		)
	}

	// Parse cluster peer URLs into routes for NATS route mesh.
	if len(cfg.ClusterPeers) > 0 {
		routes := make([]*url.URL, 0, len(cfg.ClusterPeers))
		for _, peer := range cfg.ClusterPeers {
			u, err := url.Parse(peer)
			if err != nil {
				return nil, fmt.Errorf("parsing cluster peer URL %q: %w", peer, err)
			}
			routes = append(routes, u)
		}
		opts.Routes = routes
		logger.Info("NATS cluster routes configured",
			"peers", cfg.ClusterPeers,
		)
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
func (s *Server) Start() error {
	s.ns.Start()

	if !s.ns.ReadyForConnections(10 * time.Second) {
		return fmt.Errorf("NATS server failed to become ready within 10s")
	}

	s.logger.Info("NATS server started",
		"port", s.config.Port,
		"jetstream", s.config.JetStream.IsEnabled(),
	)

	return nil
}

// Shutdown gracefully shuts down the NATS server.
func (s *Server) Shutdown() {
	s.logger.Info("shutting down NATS server")
	s.ns.Shutdown()
	s.ns.WaitForShutdown()
}

// ClientURL returns the URL for NATS clients to connect.
func (s *Server) ClientURL() string {
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
