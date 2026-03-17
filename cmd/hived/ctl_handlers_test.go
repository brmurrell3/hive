// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brmurrell3/hive/internal/auth"
	"github.com/brmurrell3/hive/internal/metrics"
	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/testutil"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// quietLogger returns a logger that only emits at error level.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// testStore creates a temporary SQLite-backed store for testing.
func testStore(t *testing.T) *state.Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	logger := quietLogger()
	store, err := state.NewStore(path, logger)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// testHandler creates a controlHandler wired up with a real NATS connection
// and state store. The authorizer is nil (backward-compat: all ops allowed).
func testHandler(t *testing.T) (*controlHandler, *nats.Conn) {
	t.Helper()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)
	store := testStore(t)
	logger := quietLogger()
	mc := metrics.NewCollector(metrics.WithLogger(logger))

	h := &controlHandler{
		runCtx:  context.Background(),
		store:   store,
		metrics: mc,
		logger:  logger,
	}
	if err := h.subscribe(nc); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return h, nc
}

// makeEnvelopeWithToken builds a valid types.Envelope with an optional UserToken.
func makeEnvelopeWithToken(t *testing.T, payload interface{}, userToken string) []byte {
	t.Helper()
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      "test-client",
		To:        "hived",
		Type:      types.MessageTypeControl,
		Timestamp: time.Now(),
		Payload:   payloadBytes,
		UserToken: userToken,
	}
	data, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return data
}

// sendRequest sends a NATS request to the given subject with the given payload
// and returns the parsed CtlResponse.
func sendRequest(t *testing.T, nc *nats.Conn, subject string, payload interface{}) *protocol.CtlResponse {
	t.Helper()
	return sendAuthRequest(t, nc, subject, payload, "")
}

// sendAuthRequest sends a NATS request with an optional UserToken for RBAC.
func sendAuthRequest(t *testing.T, nc *nats.Conn, subject string, payload interface{}, userToken string) *protocol.CtlResponse {
	t.Helper()
	data := makeEnvelopeWithToken(t, payload, userToken)
	msg, err := nc.Request(subject, data, 5*time.Second)
	if err != nil {
		t.Fatalf("NATS request to %s: %v", subject, err)
	}
	var resp protocol.CtlResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return &resp
}

// seedNode adds a node to the store for testing.
func seedNode(t *testing.T, store *state.Store, id string, status types.NodeStatus) {
	t.Helper()
	node := &types.NodeState{
		ID:       id,
		Status:   status,
		JoinedAt: time.Now(),
		Resources: types.NodeResources{
			MemoryTotal: 8 * 1024 * 1024 * 1024,
			CPUCount:    4,
		},
	}
	if err := store.SetNode(node); err != nil {
		t.Fatalf("SetNode(%s): %v", id, err)
	}
}

// seedAdminUser adds an admin user to the store and rebuilds auth on the
// handler. Returns the raw user token for authenticated requests.
func seedAdminUser(t *testing.T, h *controlHandler, userID string) string {
	t.Helper()
	rawToken := "hive-user-test-admin-token-" + userID
	user := &auth.User{
		ID:        userID,
		Role:      auth.RoleAdmin,
		TokenHash: auth.HashToken(rawToken),
	}
	if err := h.store.AddUser(user); err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	h.rebuildAuth()
	return rawToken
}

// ---------------------------------------------------------------
// validateLabelKey / validateLabelValue tests
// ---------------------------------------------------------------

func TestValidateLabelKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{name: "empty", key: "", wantErr: true},
		{name: "valid simple", key: "app", wantErr: false},
		{name: "valid with dots", key: "app.kubernetes.io", wantErr: false},
		{name: "valid with dashes", key: "my-label", wantErr: false},
		{name: "valid with underscores", key: "my_label", wantErr: false},
		{name: "valid with slashes", key: "app.kubernetes.io/name", wantErr: false},
		{name: "valid numeric start", key: "1abc", wantErr: false},
		{name: "too long", key: strings.Repeat("a", 254), wantErr: true},
		{name: "max length ok", key: strings.Repeat("a", 253), wantErr: false},
		{name: "space", key: "has space", wantErr: true},
		{name: "exclamation mark", key: "bad!", wantErr: true},
		{name: "at sign", key: "bad@label", wantErr: true},
		{name: "hash", key: "#bad", wantErr: true},
		{name: "starts with dot", key: ".hidden", wantErr: true},
		{name: "starts with dash", key: "-invalid", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateLabelKey(tc.key)
			if tc.wantErr && err == nil {
				t.Errorf("validateLabelKey(%q) = nil, want error", tc.key)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateLabelKey(%q) = %v, want nil", tc.key, err)
			}
		})
	}
}

func TestValidateLabelValue(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "empty is valid", value: "", wantErr: false},
		{name: "simple value", value: "v1", wantErr: false},
		{name: "dots", value: "v1.2.3", wantErr: false},
		{name: "dashes", value: "my-value", wantErr: false},
		{name: "underscores", value: "my_value", wantErr: false},
		{name: "max length ok", value: strings.Repeat("a", 63), wantErr: false},
		{name: "too long", value: strings.Repeat("a", 64), wantErr: true},
		{name: "space", value: "has space", wantErr: true},
		{name: "slash", value: "a/b", wantErr: true},
		{name: "at sign", value: "bad@v", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateLabelValue(tc.value)
			if tc.wantErr && err == nil {
				t.Errorf("validateLabelValue(%q) = nil, want error", tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateLabelValue(%q) = %v, want nil", tc.value, err)
			}
		})
	}
}

// ---------------------------------------------------------------
// Node handler tests
// ---------------------------------------------------------------

func TestHandleNodesList(t *testing.T) {
	h, nc := testHandler(t)

	// Seed two nodes.
	seedNode(t, h.store, "node-a", types.NodeStatusOnline)
	seedNode(t, h.store, "node-b", types.NodeStatusPending)

	resp := sendRequest(t, nc, protocol.SubjNodeList, protocol.CtlRequest{})
	if !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}
	if resp.Data == nil {
		t.Fatal("expected data in response")
	}

	var nodes []*types.NodeState
	if err := json.Unmarshal(resp.Data, &nodes); err != nil {
		t.Fatalf("unmarshal nodes: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(nodes))
	}
}

func TestHandleNodesStatus(t *testing.T) {
	h, nc := testHandler(t)

	seedNode(t, h.store, "node-found", types.NodeStatusOnline)

	t.Run("found", func(t *testing.T) {
		resp := sendRequest(t, nc, protocol.SubjNodeStatus, protocol.CtlRequest{AgentID: "node-found"})
		if !resp.Success {
			t.Fatalf("expected success, got error: %s", resp.Error)
		}
		var node types.NodeState
		if err := json.Unmarshal(resp.Data, &node); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if node.ID != "node-found" {
			t.Fatalf("expected node-found, got %s", node.ID)
		}
	})

	t.Run("not found", func(t *testing.T) {
		resp := sendRequest(t, nc, protocol.SubjNodeStatus, protocol.CtlRequest{AgentID: "node-missing"})
		if resp.Success {
			t.Fatal("expected error for missing node")
		}
		if !strings.Contains(resp.Error, "not found") {
			t.Fatalf("expected not-found error, got: %s", resp.Error)
		}
	})

	t.Run("missing node_id", func(t *testing.T) {
		resp := sendRequest(t, nc, protocol.SubjNodeStatus, protocol.CtlRequest{AgentID: ""})
		if resp.Success {
			t.Fatal("expected error for empty node_id")
		}
	})
}

func TestHandleNodesDrain(t *testing.T) {
	h, nc := testHandler(t)

	seedNode(t, h.store, "node-drain", types.NodeStatusOnline)

	resp := sendRequest(t, nc, protocol.SubjNodeDrain, protocol.CtlRequest{AgentID: "node-drain"})
	if !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}

	// Verify the node status changed.
	n := h.store.GetNode("node-drain")
	if n == nil {
		t.Fatal("node not found after drain")
	}
	if n.Status != types.NodeStatusDraining {
		t.Fatalf("expected draining, got %s", n.Status)
	}
}

func TestHandleNodesCordon(t *testing.T) {
	h, nc := testHandler(t)

	seedNode(t, h.store, "node-cordon", types.NodeStatusOnline)

	resp := sendRequest(t, nc, protocol.SubjNodeCordon, protocol.CtlRequest{AgentID: "node-cordon"})
	if !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}

	n := h.store.GetNode("node-cordon")
	if n == nil {
		t.Fatal("node not found after cordon")
	}
	if n.Status != types.NodeStatusCordoned {
		t.Fatalf("expected cordoned, got %s", n.Status)
	}
}

func TestHandleNodesUncordon(t *testing.T) {
	h, nc := testHandler(t)

	t.Run("uncordon cordoned", func(t *testing.T) {
		seedNode(t, h.store, "node-uc1", types.NodeStatusCordoned)
		resp := sendRequest(t, nc, protocol.SubjNodeUncordon, protocol.CtlRequest{AgentID: "node-uc1"})
		if !resp.Success {
			t.Fatalf("expected success, got error: %s", resp.Error)
		}
		n := h.store.GetNode("node-uc1")
		if n.Status != types.NodeStatusOnline {
			t.Fatalf("expected online, got %s", n.Status)
		}
	})

	t.Run("uncordon draining", func(t *testing.T) {
		seedNode(t, h.store, "node-uc2", types.NodeStatusDraining)
		resp := sendRequest(t, nc, protocol.SubjNodeUncordon, protocol.CtlRequest{AgentID: "node-uc2"})
		if !resp.Success {
			t.Fatalf("expected success, got error: %s", resp.Error)
		}
		n := h.store.GetNode("node-uc2")
		if n.Status != types.NodeStatusOnline {
			t.Fatalf("expected online, got %s", n.Status)
		}
	})
}

func TestHandleNodesLabel(t *testing.T) {
	h, nc := testHandler(t)

	seedNode(t, h.store, "node-label", types.NodeStatusOnline)

	t.Run("add labels", func(t *testing.T) {
		req := nodeActionRequest{
			NodeID: "node-label",
			Labels: map[string]string{"env": "prod", "tier": "frontend"},
		}
		resp := sendRequest(t, nc, protocol.SubjNodeLabel, req)
		if !resp.Success {
			t.Fatalf("expected success, got error: %s", resp.Error)
		}
		n := h.store.GetNode("node-label")
		if n.Labels["env"] != "prod" {
			t.Fatalf("expected label env=prod, got %v", n.Labels)
		}
		if n.Labels["tier"] != "frontend" {
			t.Fatalf("expected label tier=frontend, got %v", n.Labels)
		}
	})

	t.Run("invalid label key", func(t *testing.T) {
		req := nodeActionRequest{
			NodeID: "node-label",
			Labels: map[string]string{"bad key!": "val"},
		}
		resp := sendRequest(t, nc, protocol.SubjNodeLabel, req)
		if resp.Success {
			t.Fatal("expected error for invalid label key")
		}
		if !strings.Contains(resp.Error, "invalid label key") {
			t.Fatalf("expected invalid label key error, got: %s", resp.Error)
		}
	})

	t.Run("empty labels", func(t *testing.T) {
		req := nodeActionRequest{
			NodeID: "node-label",
			Labels: map[string]string{},
		}
		resp := sendRequest(t, nc, protocol.SubjNodeLabel, req)
		if resp.Success {
			t.Fatal("expected error for empty labels")
		}
		if !strings.Contains(resp.Error, "must not be empty") {
			t.Fatalf("expected empty labels error, got: %s", resp.Error)
		}
	})

	t.Run("missing node_id", func(t *testing.T) {
		req := nodeActionRequest{
			NodeID: "",
			Labels: map[string]string{"env": "dev"},
		}
		resp := sendRequest(t, nc, protocol.SubjNodeLabel, req)
		if resp.Success {
			t.Fatal("expected error for missing node_id")
		}
	})

	t.Run("invalid label value", func(t *testing.T) {
		req := nodeActionRequest{
			NodeID: "node-label",
			Labels: map[string]string{"env": "has space"},
		}
		resp := sendRequest(t, nc, protocol.SubjNodeLabel, req)
		if resp.Success {
			t.Fatal("expected error for invalid label value")
		}
		if !strings.Contains(resp.Error, "invalid label value") {
			t.Fatalf("expected invalid label value error, got: %s", resp.Error)
		}
	})

	t.Run("node not found", func(t *testing.T) {
		req := nodeActionRequest{
			NodeID: "node-nonexistent",
			Labels: map[string]string{"env": "prod"},
		}
		resp := sendRequest(t, nc, protocol.SubjNodeLabel, req)
		if resp.Success {
			t.Fatal("expected error for non-existent node")
		}
	})
}

func TestHandleNodesUnlabel(t *testing.T) {
	h, nc := testHandler(t)

	// Seed a node with labels.
	seedNode(t, h.store, "node-unlabel", types.NodeStatusOnline)
	if err := h.store.ModifyNode("node-unlabel", func(n *types.NodeState) error {
		n.Labels = map[string]string{"env": "prod", "tier": "frontend", "team": "alpha"}
		return nil
	}); err != nil {
		t.Fatalf("ModifyNode: %v", err)
	}

	t.Run("remove labels", func(t *testing.T) {
		req := nodeActionRequest{
			NodeID: "node-unlabel",
			Keys:   []string{"env", "tier"},
		}
		resp := sendRequest(t, nc, protocol.SubjNodeUnlabel, req)
		if !resp.Success {
			t.Fatalf("expected success, got error: %s", resp.Error)
		}
		n := h.store.GetNode("node-unlabel")
		if _, ok := n.Labels["env"]; ok {
			t.Fatal("expected env label to be removed")
		}
		if _, ok := n.Labels["tier"]; ok {
			t.Fatal("expected tier label to be removed")
		}
		if n.Labels["team"] != "alpha" {
			t.Fatal("expected team label to remain")
		}
	})

	t.Run("empty keys", func(t *testing.T) {
		req := nodeActionRequest{
			NodeID: "node-unlabel",
			Keys:   []string{},
		}
		resp := sendRequest(t, nc, protocol.SubjNodeUnlabel, req)
		if resp.Success {
			t.Fatal("expected error for empty keys")
		}
		if !strings.Contains(resp.Error, "must not be empty") {
			t.Fatalf("expected keys-empty error, got: %s", resp.Error)
		}
	})

	t.Run("invalid key", func(t *testing.T) {
		req := nodeActionRequest{
			NodeID: "node-unlabel",
			Keys:   []string{"bad key!"},
		}
		resp := sendRequest(t, nc, protocol.SubjNodeUnlabel, req)
		if resp.Success {
			t.Fatal("expected error for invalid key")
		}
	})
}

func TestHandleNodesApprove(t *testing.T) {
	h, nc := testHandler(t)

	t.Run("approve pending", func(t *testing.T) {
		seedNode(t, h.store, "node-pending", types.NodeStatusPending)
		resp := sendRequest(t, nc, protocol.SubjNodeApprove, protocol.CtlRequest{AgentID: "node-pending"})
		if !resp.Success {
			t.Fatalf("expected success, got error: %s", resp.Error)
		}
		n := h.store.GetNode("node-pending")
		if n.Status != types.NodeStatusOnline {
			t.Fatalf("expected online, got %s", n.Status)
		}
	})

	t.Run("approve non-pending", func(t *testing.T) {
		seedNode(t, h.store, "node-online", types.NodeStatusOnline)
		resp := sendRequest(t, nc, protocol.SubjNodeApprove, protocol.CtlRequest{AgentID: "node-online"})
		if resp.Success {
			t.Fatal("expected error for non-pending node")
		}
		if !strings.Contains(resp.Error, "only nodes in") {
			t.Fatalf("expected status restriction error, got: %s", resp.Error)
		}
	})
}

func TestHandleNodesRemove(t *testing.T) {
	h, nc := testHandler(t)

	t.Run("remove existing", func(t *testing.T) {
		seedNode(t, h.store, "node-remove", types.NodeStatusOffline)
		resp := sendRequest(t, nc, protocol.SubjNodeRemove, protocol.CtlRequest{AgentID: "node-remove"})
		if !resp.Success {
			t.Fatalf("expected success, got error: %s", resp.Error)
		}
		n := h.store.GetNode("node-remove")
		if n != nil {
			t.Fatal("expected node to be removed")
		}
	})

	t.Run("remove non-existent", func(t *testing.T) {
		resp := sendRequest(t, nc, protocol.SubjNodeRemove, protocol.CtlRequest{AgentID: "node-ghost"})
		if resp.Success {
			t.Fatal("expected error for non-existent node")
		}
		if !strings.Contains(resp.Error, "not found") {
			t.Fatalf("expected not-found error, got: %s", resp.Error)
		}
	})

	t.Run("remove empty id", func(t *testing.T) {
		resp := sendRequest(t, nc, protocol.SubjNodeRemove, protocol.CtlRequest{AgentID: ""})
		if resp.Success {
			t.Fatal("expected error for empty id")
		}
	})
}

// ---------------------------------------------------------------
// Token handler tests
// ---------------------------------------------------------------

func TestHandleTokensCreate(t *testing.T) {
	_, nc := testHandler(t)

	t.Run("create without TTL", func(t *testing.T) {
		req := struct {
			TTL string `json:"ttl,omitempty"`
		}{}
		resp := sendRequest(t, nc, protocol.SubjTokenCreate, req)
		if !resp.Success {
			t.Fatalf("expected success, got error: %s", resp.Error)
		}
		var data map[string]string
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			t.Fatalf("unmarshal data: %v", err)
		}
		if data["token"] == "" {
			t.Fatal("expected token in response")
		}
	})

	t.Run("create with TTL", func(t *testing.T) {
		req := struct {
			TTL string `json:"ttl,omitempty"`
		}{TTL: "24h"}
		resp := sendRequest(t, nc, protocol.SubjTokenCreate, req)
		if !resp.Success {
			t.Fatalf("expected success, got error: %s", resp.Error)
		}
		var data map[string]string
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			t.Fatalf("unmarshal data: %v", err)
		}
		if data["token"] == "" {
			t.Fatal("expected token in response")
		}
	})

	t.Run("invalid TTL", func(t *testing.T) {
		req := struct {
			TTL string `json:"ttl,omitempty"`
		}{TTL: "notaduration"}
		resp := sendRequest(t, nc, protocol.SubjTokenCreate, req)
		if resp.Success {
			t.Fatal("expected error for invalid TTL")
		}
		if !strings.Contains(resp.Error, "invalid TTL") {
			t.Fatalf("expected TTL error, got: %s", resp.Error)
		}
	})
}

func TestHandleTokensList(t *testing.T) {
	_, nc := testHandler(t)

	// Create a token via the handler.
	createReq := struct {
		TTL string `json:"ttl,omitempty"`
	}{}
	createResp := sendRequest(t, nc, protocol.SubjTokenCreate, createReq)
	if !createResp.Success {
		t.Fatalf("token create failed: %s", createResp.Error)
	}

	// List tokens.
	resp := sendRequest(t, nc, protocol.SubjTokenList, protocol.CtlRequest{})
	if !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}

	var tokens []*types.Token
	if err := json.Unmarshal(resp.Data, &tokens); err != nil {
		t.Fatalf("unmarshal tokens: %v", err)
	}
	if len(tokens) < 1 {
		t.Fatal("expected at least 1 token")
	}
}

func TestHandleTokensRevoke(t *testing.T) {
	_, nc := testHandler(t)

	// Create a token first.
	createReq := struct {
		TTL string `json:"ttl,omitempty"`
	}{}
	createResp := sendRequest(t, nc, protocol.SubjTokenCreate, createReq)
	if !createResp.Success {
		t.Fatalf("token create failed: %s", createResp.Error)
	}
	var tokenData map[string]string
	if err := json.Unmarshal(createResp.Data, &tokenData); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rawToken := tokenData["token"]
	// The prefix is the first 8 characters of the raw token.
	prefix := rawToken[:8]

	t.Run("revoke existing", func(t *testing.T) {
		req := struct {
			Prefix string `json:"prefix"`
		}{Prefix: prefix}
		resp := sendRequest(t, nc, protocol.SubjTokenRevoke, req)
		if !resp.Success {
			t.Fatalf("expected success, got error: %s", resp.Error)
		}
	})

	t.Run("revoke empty prefix", func(t *testing.T) {
		req := struct {
			Prefix string `json:"prefix"`
		}{Prefix: ""}
		resp := sendRequest(t, nc, protocol.SubjTokenRevoke, req)
		if resp.Success {
			t.Fatal("expected error for empty prefix")
		}
		if !strings.Contains(resp.Error, "must not be empty") {
			t.Fatalf("expected empty-prefix error, got: %s", resp.Error)
		}
	})

	t.Run("revoke non-existent", func(t *testing.T) {
		req := struct {
			Prefix string `json:"prefix"`
		}{Prefix: "zzzzzzzz"}
		resp := sendRequest(t, nc, protocol.SubjTokenRevoke, req)
		if resp.Success {
			t.Fatal("expected error for non-existent token")
		}
		if !strings.Contains(resp.Error, "no token found") {
			t.Fatalf("expected not-found error, got: %s", resp.Error)
		}
	})
}

// ---------------------------------------------------------------
// User handler tests
// ---------------------------------------------------------------

func TestHandleUsersCreate(t *testing.T) {
	// Each subtest uses its own handler to avoid RBAC side effects from
	// rebuildAuth() being called after creating the first user.

	t.Run("create valid user", func(t *testing.T) {
		_, nc := testHandler(t)
		req := userCreateRequest{
			UserID: "test-user",
			Role:   "admin",
			Teams:  []string{"team-a"},
		}
		resp := sendRequest(t, nc, protocol.SubjUserCreate, req)
		if !resp.Success {
			t.Fatalf("expected success, got error: %s", resp.Error)
		}
		var data map[string]string
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if data["user_id"] != "test-user" {
			t.Fatalf("expected user_id=test-user, got %s", data["user_id"])
		}
		if data["role"] != "admin" {
			t.Fatalf("expected role=admin, got %s", data["role"])
		}
		if data["token"] == "" {
			t.Fatal("expected token in response")
		}
	})

	t.Run("create with invalid role", func(t *testing.T) {
		_, nc := testHandler(t)
		req := userCreateRequest{
			UserID: "test-user-badrole",
			Role:   "superadmin",
		}
		resp := sendRequest(t, nc, protocol.SubjUserCreate, req)
		if resp.Success {
			t.Fatal("expected error for invalid role")
		}
		if !strings.Contains(resp.Error, "invalid role") {
			t.Fatalf("expected role error, got: %s", resp.Error)
		}
	})

	t.Run("create with empty user_id", func(t *testing.T) {
		_, nc := testHandler(t)
		req := userCreateRequest{
			UserID: "",
			Role:   "viewer",
		}
		resp := sendRequest(t, nc, protocol.SubjUserCreate, req)
		if resp.Success {
			t.Fatal("expected error for empty user_id")
		}
	})

	t.Run("create with RBAC enabled", func(t *testing.T) {
		h, nc := testHandler(t)
		adminToken := seedAdminUser(t, h, "admin-seed")

		req := userCreateRequest{
			UserID: "new-user",
			Role:   "viewer",
		}
		resp := sendAuthRequest(t, nc, protocol.SubjUserCreate, req, adminToken)
		if !resp.Success {
			t.Fatalf("expected success with admin token, got error: %s", resp.Error)
		}
		var data map[string]string
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if data["user_id"] != "new-user" {
			t.Fatalf("expected user_id=new-user, got %s", data["user_id"])
		}
	})

	t.Run("create without token when RBAC enabled", func(t *testing.T) {
		h, nc := testHandler(t)
		seedAdminUser(t, h, "admin-seed2")

		req := userCreateRequest{
			UserID: "no-auth-user",
			Role:   "viewer",
		}
		resp := sendRequest(t, nc, protocol.SubjUserCreate, req)
		if resp.Success {
			t.Fatal("expected error when RBAC is enabled and no token provided")
		}
		if !strings.Contains(resp.Error, "unauthorized") {
			t.Fatalf("expected unauthorized error, got: %s", resp.Error)
		}
	})
}

func TestHandleUsersList(t *testing.T) {
	h, nc := testHandler(t)

	// Add a user directly to the store (not via handler, so RBAC stays disabled).
	user := &auth.User{
		ID:        "list-user",
		Role:      auth.RoleViewer,
		TokenHash: auth.HashToken("dummy-token"),
	}
	if err := h.store.AddUser(user); err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	// Note: we intentionally do NOT call rebuildAuth() here, so the
	// authorizer stays nil and requests pass without a token.

	resp := sendRequest(t, nc, protocol.SubjUserList, protocol.CtlRequest{})
	if !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}

	var users []*auth.User
	if err := json.Unmarshal(resp.Data, &users); err != nil {
		t.Fatalf("unmarshal users: %v", err)
	}
	if len(users) < 1 {
		t.Fatal("expected at least 1 user")
	}

	found := false
	for _, u := range users {
		if u.ID == "list-user" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected to find list-user in results")
	}
}

func TestHandleUsersUpdate(t *testing.T) {
	// Each subtest that mutates users uses authenticated requests via an
	// admin user to avoid RBAC lockout from rebuildAuth() side effects.

	t.Run("update role", func(t *testing.T) {
		h, nc := testHandler(t)
		adminToken := seedAdminUser(t, h, "admin-upd")

		user := &auth.User{
			ID:        "update-user",
			Role:      auth.RoleViewer,
			TokenHash: auth.HashToken("dummy-token-update"),
			Teams:     []string{"team-a"},
		}
		if err := h.store.AddUser(user); err != nil {
			t.Fatalf("AddUser: %v", err)
		}
		h.rebuildAuth()

		req := userUpdateRequest{
			UserID: "update-user",
			Role:   "operator",
		}
		resp := sendAuthRequest(t, nc, protocol.SubjUserUpdate, req, adminToken)
		if !resp.Success {
			t.Fatalf("expected success, got error: %s", resp.Error)
		}
		users := h.store.AllUsers()
		for _, u := range users {
			if u.ID == "update-user" {
				if u.Role != auth.RoleOperator {
					t.Fatalf("expected role operator, got %s", u.Role)
				}
				return
			}
		}
		t.Fatal("user not found after update")
	})

	t.Run("update teams", func(t *testing.T) {
		h, nc := testHandler(t)
		adminToken := seedAdminUser(t, h, "admin-teams")

		user := &auth.User{
			ID:        "update-user",
			Role:      auth.RoleViewer,
			TokenHash: auth.HashToken("dummy-token-teams"),
			Teams:     []string{"team-a"},
		}
		if err := h.store.AddUser(user); err != nil {
			t.Fatalf("AddUser: %v", err)
		}
		h.rebuildAuth()

		req := userUpdateRequest{
			UserID: "update-user",
			Teams:  []string{"team-b", "team-c"},
		}
		resp := sendAuthRequest(t, nc, protocol.SubjUserUpdate, req, adminToken)
		if !resp.Success {
			t.Fatalf("expected success, got error: %s", resp.Error)
		}
		users := h.store.AllUsers()
		for _, u := range users {
			if u.ID == "update-user" {
				if len(u.Teams) != 2 {
					t.Fatalf("expected 2 teams, got %v", u.Teams)
				}
				return
			}
		}
		t.Fatal("user not found after update")
	})

	t.Run("clear teams", func(t *testing.T) {
		h, nc := testHandler(t)
		adminToken := seedAdminUser(t, h, "admin-clear")

		user := &auth.User{
			ID:        "update-user",
			Role:      auth.RoleViewer,
			TokenHash: auth.HashToken("dummy-token-clear"),
			Teams:     []string{"team-a"},
		}
		if err := h.store.AddUser(user); err != nil {
			t.Fatalf("AddUser: %v", err)
		}
		h.rebuildAuth()

		req := userUpdateRequest{
			UserID:     "update-user",
			ClearTeams: true,
		}
		resp := sendAuthRequest(t, nc, protocol.SubjUserUpdate, req, adminToken)
		if !resp.Success {
			t.Fatalf("expected success, got error: %s", resp.Error)
		}
		users := h.store.AllUsers()
		for _, u := range users {
			if u.ID == "update-user" {
				if len(u.Teams) != 0 {
					t.Fatalf("expected empty teams, got %v", u.Teams)
				}
				return
			}
		}
		t.Fatal("user not found after update")
	})

	t.Run("invalid role", func(t *testing.T) {
		_, nc := testHandler(t)
		req := userUpdateRequest{
			UserID: "update-user",
			Role:   "superadmin",
		}
		resp := sendRequest(t, nc, protocol.SubjUserUpdate, req)
		if resp.Success {
			t.Fatal("expected error for invalid role")
		}
	})

	t.Run("non-existent user", func(t *testing.T) {
		_, nc := testHandler(t)
		req := userUpdateRequest{
			UserID: "ghost-user",
			Role:   "admin",
		}
		resp := sendRequest(t, nc, protocol.SubjUserUpdate, req)
		if resp.Success {
			t.Fatal("expected error for non-existent user")
		}
	})
}

func TestHandleUsersRevoke(t *testing.T) {
	t.Run("revoke existing", func(t *testing.T) {
		h, nc := testHandler(t)
		user := &auth.User{
			ID:        "revoke-user",
			Role:      auth.RoleViewer,
			TokenHash: auth.HashToken("dummy-token-revoke"),
		}
		if err := h.store.AddUser(user); err != nil {
			t.Fatalf("AddUser: %v", err)
		}

		resp := sendRequest(t, nc, protocol.SubjUserRevoke, protocol.CtlRequest{AgentID: "revoke-user"})
		if !resp.Success {
			t.Fatalf("expected success, got error: %s", resp.Error)
		}
		// Verify the user is gone.
		users := h.store.AllUsers()
		for _, u := range users {
			if u.ID == "revoke-user" {
				t.Fatal("expected user to be removed")
			}
		}
	})

	t.Run("revoke non-existent", func(t *testing.T) {
		_, nc := testHandler(t)
		resp := sendRequest(t, nc, protocol.SubjUserRevoke, protocol.CtlRequest{AgentID: "ghost-user"})
		if resp.Success {
			t.Fatal("expected error for non-existent user")
		}
	})

	t.Run("revoke empty user_id", func(t *testing.T) {
		_, nc := testHandler(t)
		resp := sendRequest(t, nc, protocol.SubjUserRevoke, protocol.CtlRequest{AgentID: ""})
		if resp.Success {
			t.Fatal("expected error for empty user_id")
		}
	})
}

func TestHandleUsersRotate(t *testing.T) {
	t.Run("rotate existing", func(t *testing.T) {
		h, nc := testHandler(t)
		user := &auth.User{
			ID:        "rotate-user",
			Role:      auth.RoleAdmin,
			TokenHash: auth.HashToken("dummy-token-rotate"),
		}
		if err := h.store.AddUser(user); err != nil {
			t.Fatalf("AddUser: %v", err)
		}

		resp := sendRequest(t, nc, protocol.SubjUserRotate, protocol.CtlRequest{AgentID: "rotate-user"})
		if !resp.Success {
			t.Fatalf("expected success, got error: %s", resp.Error)
		}
		var data map[string]string
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if data["token"] == "" {
			t.Fatal("expected token in response")
		}
		// Verify the hash changed.
		users := h.store.AllUsers()
		for _, u := range users {
			if u.ID == "rotate-user" {
				if u.TokenHash == auth.HashToken("dummy-token-rotate") {
					t.Fatal("expected token hash to change after rotation")
				}
				return
			}
		}
		t.Fatal("user not found after rotate")
	})

	t.Run("rotate non-existent", func(t *testing.T) {
		_, nc := testHandler(t)
		resp := sendRequest(t, nc, protocol.SubjUserRotate, protocol.CtlRequest{AgentID: "ghost-user"})
		if resp.Success {
			t.Fatal("expected error for non-existent user")
		}
	})
}

// ---------------------------------------------------------------
// Cluster status handler test
// ---------------------------------------------------------------

func TestHandleClusterStatus(t *testing.T) {
	h, nc := testHandler(t)

	// This handler needs a valid clusterRoot with cluster.yaml and teams/.
	clusterRoot := t.TempDir()
	h.clusterRoot = clusterRoot

	// Write minimal cluster.yaml.
	clusterYAML := `apiVersion: hive/v1
kind: Cluster
metadata:
  name: test-cluster
spec:
  nats:
    port: 4222
`
	if err := os.WriteFile(filepath.Join(clusterRoot, "cluster.yaml"), []byte(clusterYAML), 0644); err != nil {
		t.Fatalf("write cluster.yaml: %v", err)
	}

	// Create teams directory (empty = 0 teams).
	teamsDir := filepath.Join(clusterRoot, "teams")
	if err := os.MkdirAll(teamsDir, 0755); err != nil {
		t.Fatalf("mkdir teams: %v", err)
	}

	// Seed some nodes.
	seedNode(t, h.store, "status-node-1", types.NodeStatusOnline)

	resp := sendRequest(t, nc, protocol.SubjStatus, protocol.CtlRequest{})
	if !resp.Success {
		t.Fatalf("expected success, got error: %s", resp.Error)
	}

	var status clusterStatusResponse
	if err := json.Unmarshal(resp.Data, &status); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if status.ClusterName != "test-cluster" {
		t.Fatalf("expected cluster name test-cluster, got %s", status.ClusterName)
	}
	if status.NodeCount != 1 {
		t.Fatalf("expected 1 node, got %d", status.NodeCount)
	}
	if status.NATSPort != 4222 {
		t.Fatalf("expected nats port 4222, got %d", status.NATSPort)
	}
}

// ---------------------------------------------------------------
// Invalid envelope / request tests
// ---------------------------------------------------------------

func TestInvalidEnvelope(t *testing.T) {
	_, nc := testHandler(t)

	// Send garbage data.
	msg, err := nc.Request(protocol.SubjNodeList, []byte("not json"), 5*time.Second)
	if err != nil {
		t.Fatalf("NATS request: %v", err)
	}
	var resp protocol.CtlResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Success {
		t.Fatal("expected error for invalid envelope")
	}
	if !strings.Contains(resp.Error, "invalid request") {
		t.Fatalf("expected invalid-request error, got: %s", resp.Error)
	}
}

func TestIncompleteEnvelope(t *testing.T) {
	_, nc := testHandler(t)

	// Send an envelope missing required fields.
	env := map[string]interface{}{
		"id":   types.NewUUID(),
		"from": "test-client",
		// missing "to", "type", "timestamp"
	}
	data, _ := json.Marshal(env)
	msg, err := nc.Request(protocol.SubjNodeList, data, 5*time.Second)
	if err != nil {
		t.Fatalf("NATS request: %v", err)
	}
	var resp protocol.CtlResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Success {
		t.Fatal("expected error for incomplete envelope")
	}
}
