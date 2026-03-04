//go:build integration

package logs

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivehq/hive/internal/testutil"
	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// publishLogEntry publishes a log entry as a proper Hive envelope on NATS.
func publishLogEntry(t *testing.T, nc *nats.Conn, entry LogEntry) {
	t.Helper()

	env := types.Envelope{
		ID:        "test-id-1",
		From:      entry.AgentID,
		To:        "hive.logs." + entry.AgentID,
		Type:      types.MessageTypeStatus,
		Timestamp: entry.Timestamp,
		Payload:   entry,
	}

	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshaling envelope: %v", err)
	}

	if err := nc.Publish("hive.logs."+entry.AgentID, data); err != nil {
		t.Fatalf("publishing log entry: %v", err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatalf("flushing NATS: %v", err)
	}
}

func TestAggregator_ReceivesLogs(t *testing.T) {
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	logDir := t.TempDir()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	agg := NewAggregator(AggregatorConfig{
		NATSConn:      nc,
		LogDir:        logDir,
		RetentionDays: 30,
		Logger:        logger,
	})

	if err := agg.Start(); err != nil {
		t.Fatalf("starting aggregator: %v", err)
	}
	defer agg.Stop()

	now := time.Now().UTC()
	entry := LogEntry{
		AgentID:   "agent-1",
		Timestamp: now,
		Level:     "info",
		Message:   "test log message",
		Fields:    map[string]interface{}{"key": "value"},
	}

	publishLogEntry(t, nc, entry)

	// Wait for the message to be processed.
	time.Sleep(500 * time.Millisecond)

	// Check that the log file was created.
	date := now.Format("2006-01-02")
	logFile := filepath.Join(logDir, "agent-1", date+".jsonl")

	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("reading log file %s: %v", logFile, err)
	}

	if len(data) == 0 {
		t.Fatal("log file is empty")
	}

	var written LogEntry
	if err := json.Unmarshal(data[:len(data)-1], &written); err != nil { // -1 for trailing newline
		t.Fatalf("parsing written log entry: %v", err)
	}

	if written.AgentID != "agent-1" {
		t.Errorf("expected agent_id=agent-1, got %s", written.AgentID)
	}
	if written.Level != "info" {
		t.Errorf("expected level=info, got %s", written.Level)
	}
	if written.Message != "test log message" {
		t.Errorf("expected message='test log message', got %s", written.Message)
	}
}

func TestAggregator_Query(t *testing.T) {
	logDir := t.TempDir()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// Pre-write some log entries directly to files.
	agentDir := filepath.Join(logDir, "agent-query")
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatalf("creating agent dir: %v", err)
	}

	now := time.Now().UTC()
	entries := []LogEntry{
		{AgentID: "agent-query", Timestamp: now.Add(-3 * time.Hour), Level: "info", Message: "first"},
		{AgentID: "agent-query", Timestamp: now.Add(-2 * time.Hour), Level: "warn", Message: "second"},
		{AgentID: "agent-query", Timestamp: now.Add(-1 * time.Hour), Level: "error", Message: "third"},
		{AgentID: "agent-query", Timestamp: now, Level: "info", Message: "fourth"},
	}

	date := now.Format("2006-01-02")
	logFile := filepath.Join(agentDir, date+".jsonl")
	f, err := os.Create(logFile)
	if err != nil {
		t.Fatalf("creating log file: %v", err)
	}

	for _, entry := range entries {
		data, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshaling entry: %v", err)
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			t.Fatalf("writing entry: %v", err)
		}
	}
	f.Close()

	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	agg := NewAggregator(AggregatorConfig{
		NATSConn: nc,
		LogDir:   logDir,
		Logger:   logger,
	})

	// Query all entries.
	results, err := agg.Query("agent-query", QueryOpts{})
	if err != nil {
		t.Fatalf("querying logs: %v", err)
	}
	if len(results) != 4 {
		t.Errorf("expected 4 entries, got %d", len(results))
	}

	// Query with level filter.
	results, err = agg.Query("agent-query", QueryOpts{Level: "info"})
	if err != nil {
		t.Fatalf("querying logs with level filter: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 info entries, got %d", len(results))
	}

	// Query with time filter.
	results, err = agg.Query("agent-query", QueryOpts{
		Since: now.Add(-90 * time.Minute),
	})
	if err != nil {
		t.Fatalf("querying logs with time filter: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 recent entries, got %d", len(results))
	}

	// Query with limit.
	results, err = agg.Query("agent-query", QueryOpts{Limit: 2})
	if err != nil {
		t.Fatalf("querying logs with limit: %v", err)
	}
	if len(results) != 2 {
		t.Errorf("expected 2 entries with limit, got %d", len(results))
	}

	// Query nonexistent agent.
	results, err = agg.Query("nonexistent", QueryOpts{})
	if err != nil {
		t.Fatalf("querying nonexistent agent: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 entries for nonexistent agent, got %d", len(results))
	}
}

func TestAggregator_Follow(t *testing.T) {
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	logDir := t.TempDir()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	agg := NewAggregator(AggregatorConfig{
		NATSConn: nc,
		LogDir:   logDir,
		Logger:   logger,
	})

	if err := agg.Start(); err != nil {
		t.Fatalf("starting aggregator: %v", err)
	}
	defer agg.Stop()

	// Start following agent-follow.
	ch, cancel := agg.Follow("agent-follow")
	defer cancel()

	// Publish a log entry.
	now := time.Now().UTC()
	entry := LogEntry{
		AgentID:   "agent-follow",
		Timestamp: now,
		Level:     "info",
		Message:   "followed message",
	}

	publishLogEntry(t, nc, entry)

	// Wait for the message on the follow channel.
	select {
	case received := <-ch:
		if received.Message != "followed message" {
			t.Errorf("expected message='followed message', got %s", received.Message)
		}
		if received.AgentID != "agent-follow" {
			t.Errorf("expected agent_id='agent-follow', got %s", received.AgentID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for followed log entry")
	}
}

func TestAggregator_Rotation(t *testing.T) {
	logDir := t.TempDir()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	// Use a very small max file size to trigger rotation.
	agg := NewAggregator(AggregatorConfig{
		NATSConn:    nc,
		LogDir:      logDir,
		MaxFileSize: 50, // 50 bytes - will rotate quickly
		Logger:      logger,
	})

	// Write several entries to trigger rotation.
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		entry := LogEntry{
			AgentID:   "agent-rotate",
			Timestamp: now,
			Level:     "info",
			Message:   "rotation test message that is long enough to exceed 50 bytes",
		}
		if err := agg.writeEntry(entry); err != nil {
			t.Fatalf("writing entry %d: %v", i, err)
		}
	}

	// Check that rotated files were created.
	agentDir := filepath.Join(logDir, "agent-rotate")
	files, err := os.ReadDir(agentDir)
	if err != nil {
		t.Fatalf("reading agent dir: %v", err)
	}

	if len(files) < 2 {
		t.Errorf("expected at least 2 files (original + rotated), got %d", len(files))
		for _, f := range files {
			t.Logf("  file: %s", f.Name())
		}
	}
}
