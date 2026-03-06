//go:build integration

package capability

import (
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/testutil"
	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
)

func crossteamTestLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func setupStore(t *testing.T) *state.Store {
	t.Helper()
	tmpDir := t.TempDir()
	logger := crossteamTestLogger()
	store, err := state.NewStore(tmpDir+"/state.db", logger)
	if err != nil {
		t.Fatalf("creating store: %v", err)
	}
	return store
}

// registerCapabilityHandler subscribes to an agent's capability subject and
// responds with a success response, simulating a real agent sidecar.
func registerCapabilityHandler(t *testing.T, nc *nats.Conn, agentID, capName string) {
	t.Helper()

	subject := "hive.capabilities." + agentID + "." + capName + ".request"
	_, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		resp := types.Envelope{
			ID:        "test-response-id",
			From:      agentID,
			Type:      types.MessageTypeCapabilityResponse,
			Timestamp: time.Now().UTC(),
			Payload: map[string]interface{}{
				"capability":  capName,
				"status":      "success",
				"outputs":     map[string]interface{}{"result": "handled"},
				"duration_ms": 1,
			},
		}
		data, _ := json.Marshal(resp)
		if msg.Reply != "" {
			_ = nc.Publish(msg.Reply, data)
		}
	})
	if err != nil {
		t.Fatalf("subscribing to %s: %v", subject, err)
	}
}

func TestExposedCapabilityIsInvocableCrossTeam(t *testing.T) {
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)
	store := setupStore(t)
	logger := crossteamTestLogger()

	// Register an agent with a capability in the store.
	caps := []types.AgentCapability{
		{Name: "code-review", Description: "Reviews code"},
	}
	if err := store.RegisterCapabilities("agent-1", "team-alpha", "1", "", caps); err != nil {
		t.Fatalf("registering capabilities: %v", err)
	}

	// Simulate agent-1's capability handler responding on the internal subject.
	registerCapabilityHandler(t, nc, "agent-1", "code-review")

	// Create teams with crossTeamCapabilities exposing "code-review".
	teams := map[string]*types.TeamManifest{
		"team-alpha": {
			Metadata: types.TeamMetadata{ID: "team-alpha"},
			Spec: types.TeamSpec{
				Communication: types.TeamCommunication{
					CrossTeamCapabilities: []interface{}{"code-review"},
				},
			},
		},
	}

	router := NewCrossTeamRouter(nc, store, logger)
	if err := router.Start(teams); err != nil {
		t.Fatalf("starting cross-team router: %v", err)
	}
	defer router.Stop()

	// Give subscriptions time to propagate.
	nc.Flush()
	time.Sleep(50 * time.Millisecond)

	// Invoke the cross-team capability from another team's agent.
	req := types.Envelope{
		ID:        "cross-team-req-1",
		From:      "agent-2",
		To:        "agent-1",
		Type:      types.MessageTypeCapabilityRequest,
		Timestamp: time.Now().UTC(),
		Payload: InvocationRequest{
			Capability: "code-review",
			Inputs:     map[string]interface{}{"file": "main.go"},
		},
	}

	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshaling request: %v", err)
	}

	subject := "hive.org.capabilities.agent-1.code-review.request"
	msg, err := nc.Request(subject, data, 5*time.Second)
	if err != nil {
		t.Fatalf("cross-team request failed: %v", err)
	}

	var respEnv types.Envelope
	if err := json.Unmarshal(msg.Data, &respEnv); err != nil {
		t.Fatalf("unmarshaling response: %v", err)
	}

	// Extract the payload to check status.
	payloadBytes, _ := json.Marshal(respEnv.Payload)
	var payload map[string]interface{}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		t.Fatalf("parsing payload: %v", err)
	}

	if payload["status"] != "success" {
		t.Errorf("expected status 'success', got %q", payload["status"])
	}
}

func TestUnexposedCapabilityReturnsPermissionError(t *testing.T) {
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)
	store := setupStore(t)
	logger := crossteamTestLogger()

	// Register agent with capabilities.
	caps := []types.AgentCapability{
		{Name: "code-review", Description: "Reviews code"},
		{Name: "deploy", Description: "Deploys code"},
	}
	if err := store.RegisterCapabilities("agent-1", "team-alpha", "1", "", caps); err != nil {
		t.Fatalf("registering capabilities: %v", err)
	}

	// Only expose "code-review", NOT "deploy".
	teams := map[string]*types.TeamManifest{
		"team-alpha": {
			Metadata: types.TeamMetadata{ID: "team-alpha"},
			Spec: types.TeamSpec{
				Communication: types.TeamCommunication{
					CrossTeamCapabilities: []interface{}{"code-review"},
				},
			},
		},
	}

	router := NewCrossTeamRouter(nc, store, logger)
	if err := router.Start(teams); err != nil {
		t.Fatalf("starting cross-team router: %v", err)
	}
	defer router.Stop()

	nc.Flush()
	time.Sleep(50 * time.Millisecond)

	// Try to invoke the unexposed "deploy" capability.
	// This subject should have no subscriber, so we expect a timeout/no-responder.
	subject := "hive.org.capabilities.agent-1.deploy.request"
	req := types.Envelope{
		ID:        "cross-team-req-2",
		From:      "agent-2",
		Type:      types.MessageTypeCapabilityRequest,
		Timestamp: time.Now().UTC(),
		Payload: InvocationRequest{
			Capability: "deploy",
			Inputs:     map[string]interface{}{},
		},
	}

	data, _ := json.Marshal(req)
	_, err := nc.Request(subject, data, 500*time.Millisecond)
	if err == nil {
		t.Error("expected error for unexposed capability, but request succeeded")
	}

	// Verify "deploy" is not in the exposed set.
	if router.IsExposed("agent-1", "deploy") {
		t.Error("capability 'deploy' should NOT be exposed cross-team")
	}
	if !router.IsExposed("agent-1", "code-review") {
		t.Error("capability 'code-review' should be exposed cross-team")
	}
}

func TestAllExposureMode(t *testing.T) {
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)
	store := setupStore(t)
	logger := crossteamTestLogger()

	// Register agent with multiple capabilities.
	caps := []types.AgentCapability{
		{Name: "code-review", Description: "Reviews code"},
		{Name: "deploy", Description: "Deploys code"},
		{Name: "test", Description: "Runs tests"},
	}
	if err := store.RegisterCapabilities("agent-1", "team-alpha", "1", "", caps); err != nil {
		t.Fatalf("registering capabilities: %v", err)
	}

	// Register handlers for all capabilities.
	registerCapabilityHandler(t, nc, "agent-1", "code-review")
	registerCapabilityHandler(t, nc, "agent-1", "deploy")
	registerCapabilityHandler(t, nc, "agent-1", "test")

	// Use "all" to expose all capabilities.
	teams := map[string]*types.TeamManifest{
		"team-alpha": {
			Metadata: types.TeamMetadata{ID: "team-alpha"},
			Spec: types.TeamSpec{
				Communication: types.TeamCommunication{
					CrossTeamCapabilities: "all",
				},
			},
		},
	}

	router := NewCrossTeamRouter(nc, store, logger)
	if err := router.Start(teams); err != nil {
		t.Fatalf("starting cross-team router: %v", err)
	}
	defer router.Stop()

	nc.Flush()
	time.Sleep(50 * time.Millisecond)

	// All capabilities should be exposed.
	if !router.IsExposed("agent-1", "code-review") {
		t.Error("'code-review' should be exposed in 'all' mode")
	}
	if !router.IsExposed("agent-1", "deploy") {
		t.Error("'deploy' should be exposed in 'all' mode")
	}
	if !router.IsExposed("agent-1", "test") {
		t.Error("'test' should be exposed in 'all' mode")
	}

	// Verify we can actually invoke them.
	for _, capName := range []string{"code-review", "deploy", "test"} {
		subject := "hive.org.capabilities.agent-1." + capName + ".request"
		req := types.Envelope{
			ID:        "req-" + capName,
			From:      "agent-2",
			Type:      types.MessageTypeCapabilityRequest,
			Timestamp: time.Now().UTC(),
			Payload: InvocationRequest{
				Capability: capName,
				Inputs:     map[string]interface{}{},
			},
		}
		data, _ := json.Marshal(req)

		msg, err := nc.Request(subject, data, 5*time.Second)
		if err != nil {
			t.Errorf("cross-team request for %q failed: %v", capName, err)
			continue
		}

		var respEnv types.Envelope
		if err := json.Unmarshal(msg.Data, &respEnv); err != nil {
			t.Errorf("unmarshaling response for %q: %v", capName, err)
		}
	}
}

func TestCrossTeamToolName(t *testing.T) {
	tests := []struct {
		capability string
		teamID     string
		want       string
	}{
		{"code-review", "team-alpha", "code-review-team-alpha"},
		{"deploy", "infra", "deploy-infra"},
	}

	for _, tt := range tests {
		got := CrossTeamToolName(tt.capability, tt.teamID)
		if got != tt.want {
			t.Errorf("CrossTeamToolName(%q, %q) = %q, want %q", tt.capability, tt.teamID, got, tt.want)
		}
	}
}

func TestParseCrossTeamCapabilities(t *testing.T) {
	tests := []struct {
		name     string
		input    interface{}
		wantLen  int
		wantWild bool
	}{
		{"nil", nil, 0, false},
		{"all string", "all", 1, true},
		{"list", []interface{}{"cap-a", "cap-b"}, 2, false},
		{"empty list", []interface{}{}, 0, false},
		{"invalid type", 42, 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseCrossTeamCapabilities(tt.input)
			if len(result) != tt.wantLen {
				t.Errorf("expected %d capabilities, got %d: %v", tt.wantLen, len(result), result)
			}
			if tt.wantWild && (len(result) == 0 || result[0] != "*") {
				t.Error("expected wildcard '*' in result")
			}
		})
	}
}
