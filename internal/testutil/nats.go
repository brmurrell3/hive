package testutil

import (
	"log/slog"
	"os"
	"testing"

	hivenats "github.com/hivehq/hive/internal/nats"
	"github.com/hivehq/hive/internal/types"
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
