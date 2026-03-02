// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build integration

package health

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/testutil"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testStore(t *testing.T) *state.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	store, err := state.NewStore(path, testLogger())
	if err != nil {
		t.Fatalf("creating test store: %v", err)
	}
	return store
}

// publishHeartbeat publishes a well-formed heartbeat envelope for the given agentID.
func publishHeartbeat(t *testing.T, nc *nats.Conn, agentID string, healthy bool) {
	t.Helper()

	hp, err := json.Marshal(types.HealthPayload{
		Healthy:       healthy,
		UptimeSeconds: 10,
		Tier:          "vm",
	})
	if err != nil {
		t.Fatalf("marshaling health payload: %v", err)
	}

	env := types.Envelope{
		ID:        "test-heartbeat-id",
		From:      agentID,
		To:        "hive.health." + agentID,
		Type:      types.MessageTypeHealth,
		Timestamp: time.Now().UTC(),
		Payload:   hp,
	}

	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshaling heartbeat: %v", err)
	}

	if err := nc.Publish("hive.health."+agentID, data); err != nil {
		t.Fatalf("publishing heartbeat: %v", err)
	}
	nc.Flush()
}

// ---------------------------------------------------------------------------
// Monitor receives heartbeats and records lastSeen time
// ---------------------------------------------------------------------------

func TestMonitor_RecordsHeartbeat(t *testing.T) {
	srv := testutil.NATSServer(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("connecting to NATS: %v", err)
	}
	defer nc.Close()

	store := testStore(t)

	// Use short intervals for testing.
	monitor := NewMonitor(store, nc, 100*time.Millisecond, 3, testLogger())
	if err := monitor.Start(); err != nil {
		t.Fatalf("starting monitor: %v", err)
	}
	defer monitor.Stop()

	// Publish a heartbeat.
	publishHeartbeat(t, nc, "agent-1", true)

	// Wait for the NATS message to be processed.
	time.Sleep(50 * time.Millisecond)

	lastSeen, ok := monitor.LastSeen("agent-1")
	if !ok {
		t.Fatal("expected agent-1 to have a lastSeen time, got not found")
	}

	if time.Since(lastSeen) > 2*time.Second {
		t.Errorf("lastSeen too far in the past: %v", lastSeen)
	}
}

// ---------------------------------------------------------------------------
// Agent marked unhealthy after maxFailures * interval with no heartbeats
// ---------------------------------------------------------------------------

func TestMonitor_AgentMarkedUnhealthyAfterTimeout(t *testing.T) {
	srv := testutil.NATSServer(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("connecting to NATS: %v", err)
	}
	defer nc.Close()

	store := testStore(t)

	// Add an agent in RUNNING state that started long enough ago to trigger
	// the "never sent heartbeat" path.
	agent := &state.AgentState{
		ID:             "timeout-agent",
		Team:           "test-team",
		Status:         state.AgentStatusRunning,
		StartedAt:      time.Now().Add(-10 * time.Second), // started well in the past
		LastTransition: time.Now(),
	}
	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("setting agent: %v", err)
	}

	// interval=100ms, maxFailures=3 -> threshold=300ms
	// The check loop runs every 100ms; after 300ms without heartbeat the agent
	// should be marked unhealthy.
	monitor := NewMonitor(store, nc, 100*time.Millisecond, 3, testLogger())
	if err := monitor.Start(); err != nil {
		t.Fatalf("starting monitor: %v", err)
	}
	defer monitor.Stop()

	// Wait long enough for at least one check to fire after threshold.
	// Threshold is 300ms; first check at 100ms sees started 10s ago > 300ms threshold.
	time.Sleep(300 * time.Millisecond)

	got := store.GetAgent("timeout-agent")
	if got == nil {
		t.Fatal("agent not found in store")
	}

	if got.Error == "" {
		t.Error("expected agent to have an error after timeout, got empty string")
	}

	expected := "heartbeat timeout: agent marked unhealthy"
	if got.Error != expected {
		t.Errorf("agent error = %q, want %q", got.Error, expected)
	}
}

// ---------------------------------------------------------------------------
// Heartbeat resets the timeout counter
// ---------------------------------------------------------------------------

func TestMonitor_HeartbeatResetsTimeout(t *testing.T) {
	srv := testutil.NATSServer(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("connecting to NATS: %v", err)
	}
	defer nc.Close()

	store := testStore(t)

	// Agent in RUNNING state, started recently enough.
	agent := &state.AgentState{
		ID:             "healthy-agent",
		Team:           "test-team",
		Status:         state.AgentStatusRunning,
		StartedAt:      time.Now().Add(-10 * time.Second),
		LastTransition: time.Now(),
	}
	if err := store.SetAgent(agent); err != nil {
		t.Fatalf("setting agent: %v", err)
	}

	// interval=100ms, maxFailures=3 -> threshold=300ms
	monitor := NewMonitor(store, nc, 100*time.Millisecond, 3, testLogger())
	if err := monitor.Start(); err != nil {
		t.Fatalf("starting monitor: %v", err)
	}
	defer monitor.Stop()

	// Send heartbeats faster than the threshold. The agent should stay healthy.
	// We send a heartbeat every 80ms for 500ms total (>threshold of 300ms).
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 6; i++ {
			publishHeartbeat(t, nc, "healthy-agent", true)
			time.Sleep(80 * time.Millisecond)
		}
	}()

	<-done

	// Give one more check cycle to be safe.
	time.Sleep(150 * time.Millisecond)

	got := store.GetAgent("healthy-agent")
	if got == nil {
		t.Fatal("agent not found in store")
	}

	if got.Error != "" {
		t.Errorf("agent should still be healthy, but has error: %q", got.Error)
	}
}

// ---------------------------------------------------------------------------
// Monitor handles malformed messages gracefully
// ---------------------------------------------------------------------------

func TestMonitor_MalformedMessage(t *testing.T) {
	srv := testutil.NATSServer(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("connecting to NATS: %v", err)
	}
	defer nc.Close()

	store := testStore(t)

	monitor := NewMonitor(store, nc, 100*time.Millisecond, 3, testLogger())
	if err := monitor.Start(); err != nil {
		t.Fatalf("starting monitor: %v", err)
	}
	defer monitor.Stop()

	// Publish invalid JSON.
	if err := nc.Publish("hive.health.bad-agent", []byte("{not valid json")); err != nil {
		t.Fatalf("publishing malformed message: %v", err)
	}
	nc.Flush()

	// Publish valid JSON but wrong message type.
	wrongType := types.Envelope{
		ID:        "wrong-type",
		From:      "bad-agent",
		To:        "hive.health.bad-agent",
		Type:      types.MessageTypeTask, // not a health message
		Timestamp: time.Now().UTC(),
	}
	data, err := json.Marshal(wrongType)
	if err != nil {
		t.Fatalf("marshaling wrong-type message: %v", err)
	}
	if err := nc.Publish("hive.health.bad-agent", data); err != nil {
		t.Fatalf("publishing wrong-type message: %v", err)
	}
	nc.Flush()

	// Publish valid health message but with empty From field.
	emptyFrom := types.Envelope{
		ID:        "empty-from",
		From:      "",
		To:        "hive.health.empty",
		Type:      types.MessageTypeHealth,
		Timestamp: time.Now().UTC(),
	}
	data, err = json.Marshal(emptyFrom)
	if err != nil {
		t.Fatalf("marshaling empty-from message: %v", err)
	}
	if err := nc.Publish("hive.health.empty", data); err != nil {
		t.Fatalf("publishing empty-from message: %v", err)
	}
	nc.Flush()

	// Wait for messages to be processed.
	time.Sleep(100 * time.Millisecond)

	// None of these malformed messages should have recorded a lastSeen entry.
	if _, ok := monitor.LastSeen("bad-agent"); ok {
		t.Error("malformed messages should not record lastSeen for bad-agent")
	}
	if _, ok := monitor.LastSeen(""); ok {
		t.Error("empty-from message should not record lastSeen for empty string")
	}
}

// ---------------------------------------------------------------------------
// RecordHeartbeat (manual, non-NATS path)
// ---------------------------------------------------------------------------

func TestMonitor_RecordHeartbeat_Manual(t *testing.T) {
	srv := testutil.NATSServer(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("connecting to NATS: %v", err)
	}
	defer nc.Close()

	store := testStore(t)

	monitor := NewMonitor(store, nc, 100*time.Millisecond, 3, testLogger())

	// RecordHeartbeat works even without Start().
	monitor.RecordHeartbeat("manual-agent")

	lastSeen, ok := monitor.LastSeen("manual-agent")
	if !ok {
		t.Fatal("expected manual-agent to have a lastSeen time")
	}

	if time.Since(lastSeen) > 2*time.Second {
		t.Errorf("lastSeen too far in the past: %v", lastSeen)
	}
}

// ---------------------------------------------------------------------------
// LastSeen returns false for unknown agent
// ---------------------------------------------------------------------------

func TestMonitor_LastSeen_UnknownAgent(t *testing.T) {
	srv := testutil.NATSServer(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("connecting to NATS: %v", err)
	}
	defer nc.Close()

	store := testStore(t)

	monitor := NewMonitor(store, nc, 100*time.Millisecond, 3, testLogger())

	_, ok := monitor.LastSeen("unknown-agent")
	if ok {
		t.Error("expected ok=false for unknown agent, got true")
	}
}

// ---------------------------------------------------------------------------
// Multiple heartbeats update lastSeen to the most recent time
// ---------------------------------------------------------------------------

func TestMonitor_MultipleHeartbeatsUpdateLastSeen(t *testing.T) {
	srv := testutil.NATSServer(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("connecting to NATS: %v", err)
	}
	defer nc.Close()

	store := testStore(t)

	monitor := NewMonitor(store, nc, 100*time.Millisecond, 3, testLogger())
	if err := monitor.Start(); err != nil {
		t.Fatalf("starting monitor: %v", err)
	}
	defer monitor.Stop()

	// First heartbeat.
	publishHeartbeat(t, nc, "multi-agent", true)
	time.Sleep(50 * time.Millisecond)

	firstSeen, ok := monitor.LastSeen("multi-agent")
	if !ok {
		t.Fatal("expected multi-agent to have a lastSeen time after first heartbeat")
	}

	// Wait a bit, then send second heartbeat.
	time.Sleep(100 * time.Millisecond)
	publishHeartbeat(t, nc, "multi-agent", true)
	time.Sleep(50 * time.Millisecond)

	secondSeen, ok := monitor.LastSeen("multi-agent")
	if !ok {
		t.Fatal("expected multi-agent to have a lastSeen time after second heartbeat")
	}

	if !secondSeen.After(firstSeen) {
		t.Errorf("second lastSeen (%v) should be after first (%v)", secondSeen, firstSeen)
	}
}

// ---------------------------------------------------------------------------
// Monitor.Stop is safe to call
// ---------------------------------------------------------------------------

func TestMonitor_StopIsSafe(t *testing.T) {
	srv := testutil.NATSServer(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("connecting to NATS: %v", err)
	}
	defer nc.Close()

	store := testStore(t)

	monitor := NewMonitor(store, nc, 100*time.Millisecond, 3, testLogger())
	if err := monitor.Start(); err != nil {
		t.Fatalf("starting monitor: %v", err)
	}

	// Stop should not panic or block indefinitely.
	monitor.Stop()
}

// ---------------------------------------------------------------------------
// Default interval and maxFailures
// ---------------------------------------------------------------------------

func TestMonitor_Defaults(t *testing.T) {
	srv := testutil.NATSServer(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("connecting to NATS: %v", err)
	}
	defer nc.Close()

	store := testStore(t)

	// Pass zero values; the constructor should apply defaults.
	monitor := NewMonitor(store, nc, 0, 0, testLogger())

	if monitor.interval != 30*time.Second {
		t.Errorf("default interval = %v, want 30s", monitor.interval)
	}
	if monitor.maxFailures != 3 {
		t.Errorf("default maxFailures = %d, want 3", monitor.maxFailures)
	}
}
