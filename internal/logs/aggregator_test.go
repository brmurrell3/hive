// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build integration

package logs

import (
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/brmurrell3/hive/internal/testutil"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// publishLogEntry publishes a log entry as a proper Hive envelope on NATS.
func publishLogEntry(t *testing.T, nc *nats.Conn, entry LogEntry) {
	t.Helper()

	entryPayload, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshaling log entry payload: %v", err)
	}

	env := types.Envelope{
		ID:        "test-id-1",
		From:      entry.AgentID,
		To:        "hive.logs." + entry.AgentID,
		Type:      types.MessageTypeStatus,
		Timestamp: entry.Timestamp,
		Payload:   entryPayload,
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

	// Wait for the message to be processed (buffered write queue flushes every 500ms).
	time.Sleep(2 * time.Second)

	// Query the database to verify the entry was stored.
	results, err := agg.Query("agent-1", QueryOpts{})
	if err != nil {
		t.Fatalf("querying logs: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(results))
	}

	written := results[0]
	if written.AgentID != "agent-1" {
		t.Errorf("expected agent_id=agent-1, got %s", written.AgentID)
	}
	if written.Level != "info" {
		t.Errorf("expected level=info, got %s", written.Level)
	}
	if written.Message != "test log message" {
		t.Errorf("expected message='test log message', got %s", written.Message)
	}
	if written.Fields["key"] != "value" {
		t.Errorf("expected fields[key]=value, got %v", written.Fields["key"])
	}
}

func TestAggregator_Query(t *testing.T) {
	logDir := t.TempDir()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	agg := NewAggregator(AggregatorConfig{
		NATSConn: nc,
		LogDir:   logDir,
		Logger:   logger,
	})

	if err := agg.Start(); err != nil {
		t.Fatalf("starting aggregator: %v", err)
	}
	defer agg.Stop()

	// Insert entries directly via writeEntry.
	now := time.Now().UTC()
	entries := []LogEntry{
		{AgentID: "agent-query", Timestamp: now.Add(-3 * time.Hour), Level: "info", Message: "first"},
		{AgentID: "agent-query", Timestamp: now.Add(-2 * time.Hour), Level: "warn", Message: "second"},
		{AgentID: "agent-query", Timestamp: now.Add(-1 * time.Hour), Level: "error", Message: "third"},
		{AgentID: "agent-query", Timestamp: now, Level: "info", Message: "fourth"},
	}

	for i, entry := range entries {
		if err := agg.writeEntry(entry); err != nil {
			t.Fatalf("writing entry %d: %v", i, err)
		}
	}

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
	ch, cancel, err := agg.Follow("agent-follow")
	if err != nil {
		t.Fatalf("following agent: %v", err)
	}
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

func TestAggregator_Retention(t *testing.T) {
	logDir := t.TempDir()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	agg := NewAggregator(AggregatorConfig{
		NATSConn:      nc,
		LogDir:        logDir,
		RetentionDays: 1, // 1 day retention
		Logger:        logger,
	})

	if err := agg.Start(); err != nil {
		t.Fatalf("starting aggregator: %v", err)
	}
	defer agg.Stop()

	now := time.Now().UTC()

	// Write an old entry (beyond retention) and a recent one.
	old := LogEntry{
		AgentID:   "agent-ret",
		Timestamp: now.Add(-48 * time.Hour),
		Level:     "info",
		Message:   "old message",
	}
	recent := LogEntry{
		AgentID:   "agent-ret",
		Timestamp: now,
		Level:     "info",
		Message:   "recent message",
	}

	if err := agg.writeEntry(old); err != nil {
		t.Fatalf("writing old entry: %v", err)
	}
	if err := agg.writeEntry(recent); err != nil {
		t.Fatalf("writing recent entry: %v", err)
	}

	// Run retention cleanup.
	if err := agg.cleanRetention(); err != nil {
		t.Fatalf("cleaning retention: %v", err)
	}

	// Only the recent entry should remain.
	results, err := agg.Query("agent-ret", QueryOpts{})
	if err != nil {
		t.Fatalf("querying logs: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 entry after retention cleanup, got %d", len(results))
	}
	if results[0].Message != "recent message" {
		t.Errorf("expected 'recent message', got %s", results[0].Message)
	}
}
