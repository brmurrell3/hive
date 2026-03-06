package testutil

import (
	"log/slog"
	"os"
	"testing"

	hivenats "github.com/hivehq/hive/internal/nats"
	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// NATSServer starts an embedded NATS server on a random port for testing.
// The server is automatically shut down when the test completes.
func NATSServer(t *testing.T) *hivenats.Server {
	t.Helper()

	tmpDir := t.TempDir()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	cfg := types.NATSConfig{
		Port: -1, // random port
		JetStream: types.JetStreamConfig{
			StorePath: tmpDir,
		},
	}

	srv, err := hivenats.NewServer(cfg, tmpDir, logger)
	if err != nil {
		t.Fatalf("creating test NATS server: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("starting test NATS server: %v", err)
	}

	t.Cleanup(func() {
		srv.Shutdown()
	})

	return srv
}

// NATSConnect creates a NATS client connection to the test server.
// The connection is automatically closed when the test completes.
func NATSConnect(t *testing.T, srv *hivenats.Server) *nats.Conn {
	t.Helper()

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("connecting to test NATS: %v", err)
	}

	t.Cleanup(func() {
		nc.Close()
	})

	return nc
}
