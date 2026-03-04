// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build integration

package node

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

func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testStateStore(t *testing.T) *state.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	store, err := state.NewStore(path, testLogger(t))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

// addTestToken adds a valid token to the store and returns the raw token string.
func addTestToken(t *testing.T, store *state.Store) string {
	t.Helper()
	raw := "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	hash := state.HashToken(raw)
	tok := &types.Token{
		Prefix:    raw[:8],
		Hash:      hash,
		CreatedAt: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
	}
	if err := store.AddToken(tok); err != nil {
		t.Fatalf("AddToken: %v", err)
	}
	return raw
}

// sendJoinRequest publishes a join request via NATS request-reply and returns the response.
func sendJoinRequest(t *testing.T, nc *nats.Conn, req types.JoinRequest) types.JoinResponse {
	t.Helper()

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshaling join request: %v", err)
	}

	msg, err := nc.Request("hive.join.request", data, 5*time.Second)
	if err != nil {
		t.Fatalf("NATS request: %v", err)
	}

	var resp types.JoinResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatalf("unmarshaling join response: %v", err)
	}

	return resp
}

// ---------------------------------------------------------------------------
// handleJoinRequest with valid token registers node
// ---------------------------------------------------------------------------

func TestHandleJoinRequest_ValidToken(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)
	logger := testLogger(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	reg := NewRegistry(store, nc, logger, "")
	if err := reg.Start(); err != nil {
		t.Fatalf("Registry.Start: %v", err)
	}
	defer reg.Stop()

	rawToken := addTestToken(t, store)

	resp := sendJoinRequest(t, nc, types.JoinRequest{
		Token:    rawToken,
		Hostname: "worker-1",
		Arch:     "amd64",
		Resources: types.NodeResources{
			KVMAvail:    true,
			MemoryTotal: 8 * 1024 * 1024 * 1024,
			CPUCount:    4,
		},
	})

	if !resp.Accepted {
		t.Fatalf("expected join to be accepted, got error: %s", resp.Error)
	}
	if resp.NodeID == "" {
		t.Error("expected non-empty node ID")
	}
	if resp.Tier != types.NodeTier1 {
		t.Errorf("expected tier %d, got %d", types.NodeTier1, resp.Tier)
	}

	// Verify node is stored.
	node := store.GetNode(resp.NodeID)
	if node == nil {
		t.Fatalf("node %q not found in store", resp.NodeID)
	}
	if node.Status != types.NodeStatusOnline {
		t.Errorf("node status = %q, want %q", node.Status, types.NodeStatusOnline)
	}
	if node.Arch != "amd64" {
		t.Errorf("node arch = %q, want %q", node.Arch, "amd64")
	}
	if node.Hostname != "worker-1" {
		t.Errorf("node hostname = %q, want %q", node.Hostname, "worker-1")
	}
}

// ---------------------------------------------------------------------------
// handleJoinRequest with invalid token rejects
// ---------------------------------------------------------------------------

func TestHandleJoinRequest_InvalidToken(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)
	logger := testLogger(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	reg := NewRegistry(store, nc, logger, "")
	if err := reg.Start(); err != nil {
		t.Fatalf("Registry.Start: %v", err)
	}
	defer reg.Stop()

	// Add a valid token but use a wrong one in the request.
	addTestToken(t, store)

	tests := []struct {
		name  string
		token string
	}{
		{"completely wrong token", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef"},
		{"empty token", ""},
		{"partial token", "aabbccdd"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := sendJoinRequest(t, nc, types.JoinRequest{
				Token:    tt.token,
				Hostname: "bad-node",
				Arch:     "arm64",
				Resources: types.NodeResources{
					MemoryTotal: 2 * 1024 * 1024 * 1024,
					CPUCount:    4,
				},
			})

			if resp.Accepted {
				t.Error("expected join to be rejected, but it was accepted")
			}
			if resp.Error == "" {
				t.Error("expected error message in response")
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Tier classification via join request
// ---------------------------------------------------------------------------

func TestHandleJoinRequest_TierClassification(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)
	logger := testLogger(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	reg := NewRegistry(store, nc, logger, "")
	if err := reg.Start(); err != nil {
		t.Fatalf("Registry.Start: %v", err)
	}
	defer reg.Stop()

	rawToken := addTestToken(t, store)

	tests := []struct {
		name      string
		resources types.NodeResources
		wantTier  types.NodeTier
	}{
		{
			name: "KVM + 4GB = Tier 1",
			resources: types.NodeResources{
				KVMAvail:    true,
				MemoryTotal: 4 * 1024 * 1024 * 1024,
				CPUCount:    4,
			},
			wantTier: types.NodeTier1,
		},
		{
			name: "KVM + 8GB = Tier 1",
			resources: types.NodeResources{
				KVMAvail:    true,
				MemoryTotal: 8 * 1024 * 1024 * 1024,
				CPUCount:    8,
			},
			wantTier: types.NodeTier1,
		},
		{
			name: "no KVM + 4GB = Tier 2",
			resources: types.NodeResources{
				KVMAvail:    false,
				MemoryTotal: 4 * 1024 * 1024 * 1024,
				CPUCount:    4,
			},
			wantTier: types.NodeTier2,
		},
		{
			name: "KVM + 2GB = Tier 2",
			resources: types.NodeResources{
				KVMAvail:    true,
				MemoryTotal: 2 * 1024 * 1024 * 1024,
				CPUCount:    2,
			},
			wantTier: types.NodeTier2,
		},
		{
			name: "no KVM + 1GB = Tier 2 (Pi-like)",
			resources: types.NodeResources{
				KVMAvail:    false,
				MemoryTotal: 1 * 1024 * 1024 * 1024,
				CPUCount:    4,
			},
			wantTier: types.NodeTier2,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := sendJoinRequest(t, nc, types.JoinRequest{
				Token:     rawToken,
				Hostname:  "tier-test",
				Arch:      "amd64",
				Resources: tt.resources,
				AgentID:   "", // Use unique hostnames so they don't clash
			})

			if !resp.Accepted {
				t.Fatalf("expected join to be accepted, got error: %s", resp.Error)
			}
			if resp.Tier != tt.wantTier {
				t.Errorf("test %d: tier = %d, want %d", i, resp.Tier, tt.wantTier)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// RecordHeartbeat updates last heartbeat
// ---------------------------------------------------------------------------

func TestRecordHeartbeat(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)
	logger := testLogger(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	reg := NewRegistry(store, nc, logger, "")

	// First register a node.
	pastTime := time.Now().UTC().Add(-10 * time.Minute)
	node := &types.NodeState{
		ID:            "heartbeat-node",
		Tier:          types.NodeTier1,
		Arch:          "amd64",
		Hostname:      "hb-host",
		Status:        types.NodeStatusOnline,
		Resources:     types.NodeResources{KVMAvail: true, MemoryTotal: 8 * 1024 * 1024 * 1024},
		JoinedAt:      pastTime,
		LastHeartbeat: pastTime,
	}
	if err := store.SetNode(node); err != nil {
		t.Fatalf("SetNode: %v", err)
	}

	// Record heartbeat.
	beforeHB := time.Now().UTC()
	if err := reg.RecordHeartbeat("heartbeat-node"); err != nil {
		t.Fatalf("RecordHeartbeat: %v", err)
	}

	updated := store.GetNode("heartbeat-node")
	if updated == nil {
		t.Fatal("node not found after heartbeat")
	}

	if updated.LastHeartbeat.Before(beforeHB) {
		t.Errorf("LastHeartbeat = %v, expected to be after %v", updated.LastHeartbeat, beforeHB)
	}
}

func TestRecordHeartbeat_BringsOfflineNodeOnline(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)
	logger := testLogger(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	reg := NewRegistry(store, nc, logger, "")

	// Register a node as offline.
	node := &types.NodeState{
		ID:            "offline-node",
		Tier:          types.NodeTier2,
		Arch:          "arm64",
		Hostname:      "offline-host",
		Status:        types.NodeStatusOffline,
		Resources:     types.NodeResources{MemoryTotal: 2 * 1024 * 1024 * 1024},
		JoinedAt:      time.Now().UTC().Add(-1 * time.Hour),
		LastHeartbeat: time.Now().UTC().Add(-1 * time.Hour),
	}
	if err := store.SetNode(node); err != nil {
		t.Fatalf("SetNode: %v", err)
	}

	// Record heartbeat should bring it online.
	if err := reg.RecordHeartbeat("offline-node"); err != nil {
		t.Fatalf("RecordHeartbeat: %v", err)
	}

	updated := store.GetNode("offline-node")
	if updated == nil {
		t.Fatal("node not found after heartbeat")
	}
	if updated.Status != types.NodeStatusOnline {
		t.Errorf("Status = %q, want %q after heartbeat", updated.Status, types.NodeStatusOnline)
	}
}

func TestRecordHeartbeat_UnknownNode(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)
	logger := testLogger(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	reg := NewRegistry(store, nc, logger, "")

	err = reg.RecordHeartbeat("nonexistent-node")
	if err == nil {
		t.Error("expected error for heartbeat on unknown node, got nil")
	}
}

// ---------------------------------------------------------------------------
// CheckNodes marks stale nodes offline
// ---------------------------------------------------------------------------

func TestCheckNodes_MarksStaleNodesOffline(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)
	logger := testLogger(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	reg := NewRegistry(store, nc, logger, "")

	// Create a node with a stale heartbeat.
	staleNode := &types.NodeState{
		ID:            "stale-node",
		Tier:          types.NodeTier1,
		Arch:          "amd64",
		Hostname:      "stale-host",
		Status:        types.NodeStatusOnline,
		Resources:     types.NodeResources{KVMAvail: true, MemoryTotal: 8 * 1024 * 1024 * 1024},
		JoinedAt:      time.Now().UTC().Add(-1 * time.Hour),
		LastHeartbeat: time.Now().UTC().Add(-5 * time.Minute),
	}
	if err := store.SetNode(staleNode); err != nil {
		t.Fatalf("SetNode (stale): %v", err)
	}

	// Create a fresh node.
	freshNode := &types.NodeState{
		ID:            "fresh-node",
		Tier:          types.NodeTier2,
		Arch:          "arm64",
		Hostname:      "fresh-host",
		Status:        types.NodeStatusOnline,
		Resources:     types.NodeResources{MemoryTotal: 4 * 1024 * 1024 * 1024},
		JoinedAt:      time.Now().UTC().Add(-30 * time.Minute),
		LastHeartbeat: time.Now().UTC(),
	}
	if err := store.SetNode(freshNode); err != nil {
		t.Fatalf("SetNode (fresh): %v", err)
	}

	// Check nodes with a 2-minute timeout; the stale node (5min old) should go offline.
	reg.CheckNodes(2 * time.Minute)

	stale := store.GetNode("stale-node")
	if stale == nil {
		t.Fatal("stale node not found")
	}
	if stale.Status != types.NodeStatusOffline {
		t.Errorf("stale node status = %q, want %q", stale.Status, types.NodeStatusOffline)
	}

	fresh := store.GetNode("fresh-node")
	if fresh == nil {
		t.Fatal("fresh node not found")
	}
	if fresh.Status != types.NodeStatusOnline {
		t.Errorf("fresh node status = %q, want %q", fresh.Status, types.NodeStatusOnline)
	}
}

func TestCheckNodes_DoesNotAffectAlreadyOffline(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)
	logger := testLogger(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	reg := NewRegistry(store, nc, logger, "")

	// Create an already-offline node with a stale heartbeat.
	offlineNode := &types.NodeState{
		ID:            "already-offline",
		Tier:          types.NodeTier2,
		Arch:          "arm64",
		Hostname:      "offline-host",
		Status:        types.NodeStatusOffline,
		Resources:     types.NodeResources{MemoryTotal: 2 * 1024 * 1024 * 1024},
		JoinedAt:      time.Now().UTC().Add(-2 * time.Hour),
		LastHeartbeat: time.Now().UTC().Add(-1 * time.Hour),
	}
	if err := store.SetNode(offlineNode); err != nil {
		t.Fatalf("SetNode: %v", err)
	}

	// CheckNodes should not error or change anything for already-offline nodes.
	reg.CheckNodes(1 * time.Minute)

	node := store.GetNode("already-offline")
	if node == nil {
		t.Fatal("node not found")
	}
	if node.Status != types.NodeStatusOffline {
		t.Errorf("status = %q, want %q (should remain offline)", node.Status, types.NodeStatusOffline)
	}
}

// ---------------------------------------------------------------------------
// Join request with AgentID populates node.Agents
// ---------------------------------------------------------------------------

func TestHandleJoinRequest_AgentIDPopulated(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)
	logger := testLogger(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	reg := NewRegistry(store, nc, logger, "")
	if err := reg.Start(); err != nil {
		t.Fatalf("Registry.Start: %v", err)
	}
	defer reg.Stop()

	rawToken := addTestToken(t, store)

	resp := sendJoinRequest(t, nc, types.JoinRequest{
		Token:    rawToken,
		Hostname: "agent-host",
		Arch:     "arm64",
		Resources: types.NodeResources{
			MemoryTotal: 2 * 1024 * 1024 * 1024,
			CPUCount:    4,
		},
		AgentID: "my-firmware-agent",
	})

	if !resp.Accepted {
		t.Fatalf("expected join to be accepted, got error: %s", resp.Error)
	}

	node := store.GetNode(resp.NodeID)
	if node == nil {
		t.Fatalf("node %q not found", resp.NodeID)
	}

	if len(node.Agents) != 1 || node.Agents[0] != "my-firmware-agent" {
		t.Errorf("node.Agents = %v, want [my-firmware-agent]", node.Agents)
	}
}

// ---------------------------------------------------------------------------
// Node heartbeat via NATS updates LastHeartbeat
// ---------------------------------------------------------------------------

func TestNodeHeartbeat_ViaSubscription(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)
	logger := testLogger(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	reg := NewRegistry(store, nc, logger, "")
	if err := reg.Start(); err != nil {
		t.Fatalf("Registry.Start: %v", err)
	}
	defer reg.Stop()

	// Register a node with an old heartbeat timestamp.
	pastTime := time.Now().UTC().Add(-10 * time.Minute)
	node := &types.NodeState{
		ID:            "hb-nats-node",
		Tier:          types.NodeTier1,
		Arch:          "amd64",
		Hostname:      "hb-nats-host",
		Status:        types.NodeStatusOnline,
		Resources:     types.NodeResources{KVMAvail: true, MemoryTotal: 8 * 1024 * 1024 * 1024},
		JoinedAt:      pastTime,
		LastHeartbeat: pastTime,
	}
	if err := store.SetNode(node); err != nil {
		t.Fatalf("SetNode: %v", err)
	}

	// Publish a heartbeat via NATS on the expected subject.
	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      "hb-nats-node",
		To:        "hived",
		Type:      types.MessageTypeNodeHeartbeat,
		Timestamp: time.Now().UTC(),
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	beforeHB := time.Now().UTC()
	if err := nc.Publish("hive.node.hb-nats-node.heartbeat", data); err != nil {
		t.Fatalf("NATS publish: %v", err)
	}
	nc.Flush()

	// Give the subscription handler time to process the message.
	time.Sleep(100 * time.Millisecond)

	updated := store.GetNode("hb-nats-node")
	if updated == nil {
		t.Fatal("node not found after heartbeat")
	}
	if updated.LastHeartbeat.Before(beforeHB) {
		t.Errorf("LastHeartbeat = %v, expected to be after %v", updated.LastHeartbeat, beforeHB)
	}
}

func TestNodeHeartbeat_BringsOfflineNodeOnline(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)
	logger := testLogger(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	reg := NewRegistry(store, nc, logger, "")
	if err := reg.Start(); err != nil {
		t.Fatalf("Registry.Start: %v", err)
	}
	defer reg.Stop()

	// Register a node as offline.
	node := &types.NodeState{
		ID:            "offline-nats-node",
		Tier:          types.NodeTier2,
		Arch:          "arm64",
		Hostname:      "offline-nats-host",
		Status:        types.NodeStatusOffline,
		Resources:     types.NodeResources{MemoryTotal: 2 * 1024 * 1024 * 1024},
		JoinedAt:      time.Now().UTC().Add(-1 * time.Hour),
		LastHeartbeat: time.Now().UTC().Add(-1 * time.Hour),
	}
	if err := store.SetNode(node); err != nil {
		t.Fatalf("SetNode: %v", err)
	}

	// Publish a heartbeat.
	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      "offline-nats-node",
		To:        "hived",
		Type:      types.MessageTypeNodeHeartbeat,
		Timestamp: time.Now().UTC(),
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	if err := nc.Publish("hive.node.offline-nats-node.heartbeat", data); err != nil {
		t.Fatalf("NATS publish: %v", err)
	}
	nc.Flush()
	time.Sleep(100 * time.Millisecond)

	updated := store.GetNode("offline-nats-node")
	if updated == nil {
		t.Fatal("node not found after heartbeat")
	}
	if updated.Status != types.NodeStatusOnline {
		t.Errorf("Status = %q, want %q after heartbeat", updated.Status, types.NodeStatusOnline)
	}
}

func TestNodeHeartbeat_IgnoresWrongMessageType(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)
	logger := testLogger(t)

	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	reg := NewRegistry(store, nc, logger, "")
	if err := reg.Start(); err != nil {
		t.Fatalf("Registry.Start: %v", err)
	}
	defer reg.Stop()

	// Register a node with an old heartbeat.
	pastTime := time.Now().UTC().Add(-10 * time.Minute)
	node := &types.NodeState{
		ID:            "wrong-type-node",
		Tier:          types.NodeTier1,
		Arch:          "amd64",
		Hostname:      "wrong-type-host",
		Status:        types.NodeStatusOnline,
		Resources:     types.NodeResources{KVMAvail: true, MemoryTotal: 8 * 1024 * 1024 * 1024},
		JoinedAt:      pastTime,
		LastHeartbeat: pastTime,
	}
	if err := store.SetNode(node); err != nil {
		t.Fatalf("SetNode: %v", err)
	}

	// Publish a message with the wrong type on the heartbeat subject.
	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      "wrong-type-node",
		To:        "hived",
		Type:      types.MessageTypeHealth, // wrong type, should be node-heartbeat
		Timestamp: time.Now().UTC(),
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	if err := nc.Publish("hive.node.wrong-type-node.heartbeat", data); err != nil {
		t.Fatalf("NATS publish: %v", err)
	}
	nc.Flush()
	time.Sleep(100 * time.Millisecond)

	// LastHeartbeat should NOT have been updated.
	updated := store.GetNode("wrong-type-node")
	if updated == nil {
		t.Fatal("node not found")
	}
	if !updated.LastHeartbeat.Equal(pastTime) {
		t.Errorf("LastHeartbeat = %v, expected %v (should not have changed)", updated.LastHeartbeat, pastTime)
	}
}
