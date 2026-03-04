package nats

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/hivehq/hive/internal/types"
	natsserver "github.com/nats-io/nats-server/v2/server"
)

// Server wraps an embedded NATS server.
type Server struct {
	ns     *natsserver.Server
	config types.NATSConfig
	logger *slog.Logger
}

// NewServer creates a new embedded NATS server from the cluster config.
func NewServer(cfg types.NATSConfig, dataDir string, logger *slog.Logger) (*Server, error) {
	opts := &natsserver.Options{
		Port:     cfg.Port,
		NoLog:    true,
		NoSigs:   true,
		MaxPayload: 2 * 1024 * 1024, // 2MB per spec
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

	ns, err := natsserver.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("creating NATS server: %w", err)
	}

	return &Server{
		ns:     ns,
		config: cfg,
		logger: logger,
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

// Port returns the actual port the server is listening on.
func (s *Server) Port() int {
	addr := s.ns.Addr()
	if addr == nil {
		return 0
	}
	// ClientURL() is the reliable way to get the port
	return s.config.Port
}
