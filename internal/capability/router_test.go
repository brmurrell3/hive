//go:build integration

package capability

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	hivenats "github.com/hivehq/hive/internal/nats"
	"github.com/hivehq/hive/internal/testutil"
	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testNATSConn(t *testing.T, srv *hivenats.Server) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	return nc
}

// boolPtr returns a pointer to the given bool value.
func boolPtr(b bool) *bool {
	return &b
}

// ---------------------------------------------------------------------------
// Capability invocation succeeds
// ---------------------------------------------------------------------------

func TestRouter_InvocationSucceeds(t *testing.T) {
	srv := testutil.NATSServer(t)
	ncA := testNATSConn(t, srv)
	ncB := testNATSConn(t, srv)

	routerA := NewRouter("agent-a", ncA, testLogger())
	routerB := NewRouter("agent-b", ncB, testLogger())

	// Register an echo handler on agent B that returns the inputs it receives.
	routerB.RegisterHandler("echo", func(inputs map[string]interface{}) (map[string]interface{}, error) {
		return map[string]interface{}{
			"echoed": inputs["message"],
		}, nil
	})

	if err := routerB.Start(); err != nil {
		t.Fatalf("starting router B: %v", err)
	}
	defer routerB.Stop()

	// Small delay to let the subscription propagate.
	time.Sleep(50 * time.Millisecond)

	inputs := map[string]interface{}{
		"message": "hello from A",
	}

	resp, err := routerA.Invoke("agent-b", "echo", inputs, 5*time.Second)
	if err != nil {
		t.Fatalf("invoking echo capability: %v", err)
	}

	if resp.Status != "success" {
		t.Fatalf("expected status=success, got %q", resp.Status)
	}
	if resp.Capability != "echo" {
		t.Errorf("expected capability=echo, got %q", resp.Capability)
	}
	if resp.Outputs["echoed"] != "hello from A" {
		t.Errorf("expected echoed output %q, got %v", "hello from A", resp.Outputs["echoed"])
	}
	if resp.DurationMs < 0 {
		t.Errorf("expected non-negative duration_ms, got %d", resp.DurationMs)
	}
	if resp.Error != nil {
		t.Errorf("expected no error, got %+v", resp.Error)
	}
}

// ---------------------------------------------------------------------------
// Timeout handling
// ---------------------------------------------------------------------------

func TestRouter_InvocationTimeout(t *testing.T) {
	srv := testutil.NATSServer(t)
	ncA := testNATSConn(t, srv)
	ncB := testNATSConn(t, srv)

	routerB := NewRouter("agent-b", ncB, testLogger())

	// Register a handler on agent B that never responds (sleeps longer than
	// the caller's timeout). This ensures there IS a subscriber so NATS
	// doesn't return ErrNoResponders, but the response never arrives in time.
	routerB.RegisterHandler("slow", func(inputs map[string]interface{}) (map[string]interface{}, error) {
		time.Sleep(10 * time.Second)
		return nil, nil
	})

	if err := routerB.Start(); err != nil {
		t.Fatalf("starting router B: %v", err)
	}
	defer routerB.Stop()

	time.Sleep(50 * time.Millisecond)

	routerA := NewRouter("agent-a", ncA, testLogger())

	inputs := map[string]interface{}{
		"message": "hello",
	}

	start := time.Now()
	resp, err := routerA.Invoke("agent-b", "slow", inputs, 500*time.Millisecond)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected no error from Invoke, got: %v", err)
	}

	if resp.Status != "timeout" {
		t.Fatalf("expected status=timeout, got %q", resp.Status)
	}
	if resp.Error == nil {
		t.Fatal("expected an error in the timeout response")
	}
	if resp.Error.Code != "TIMEOUT" {
		t.Errorf("expected error code TIMEOUT, got %q", resp.Error.Code)
	}
	if !resp.Error.Retryable {
		t.Error("expected timeout error to be retryable")
	}
	if resp.Capability != "slow" {
		t.Errorf("expected capability=slow, got %q", resp.Capability)
	}

	// Verify the timeout actually respected the duration we specified.
	if elapsed < 400*time.Millisecond {
		t.Errorf("timeout resolved too quickly (%v), expected at least ~500ms", elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("timeout took too long (%v), expected around 500ms", elapsed)
	}
}

// ---------------------------------------------------------------------------
// Message envelope validation
// ---------------------------------------------------------------------------

func TestRouter_EnvelopeFields(t *testing.T) {
	srv := testutil.NATSServer(t)
	ncA := testNATSConn(t, srv)
	ncB := testNATSConn(t, srv)

	// We subscribe on B's connection directly so we can inspect the raw
	// request envelope, then reply with a well-formed response envelope.
	received := make(chan *nats.Msg, 1)
	_, err := ncB.Subscribe("hive.capabilities.agent-b.echo.request", func(msg *nats.Msg) {
		received <- msg

		// Parse the incoming request envelope to get the ID for correlation.
		var reqEnv types.Envelope
		if err := json.Unmarshal(msg.Data, &reqEnv); err != nil {
			t.Errorf("unmarshaling request envelope in handler: %v", err)
			return
		}

		// Build a proper response envelope.
		respEnv := types.Envelope{
			ID:            types.NewUUID(),
			From:          "agent-b",
			To:            reqEnv.From,
			Type:          types.MessageTypeCapabilityResponse,
			Timestamp:     time.Now().UTC(),
			CorrelationID: reqEnv.ID,
			Payload: InvocationResponse{
				Capability: "echo",
				Status:     "success",
				Outputs:    map[string]interface{}{"result": "ok"},
				DurationMs: 1,
			},
		}

		data, err := json.Marshal(respEnv)
		if err != nil {
			t.Errorf("marshaling response envelope: %v", err)
			return
		}

		if err := ncB.Publish(msg.Reply, data); err != nil {
			t.Errorf("publishing response: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("subscribing to request subject: %v", err)
	}

	time.Sleep(50 * time.Millisecond)

	routerA := NewRouter("agent-a", ncA, testLogger())

	beforeInvoke := time.Now().UTC().Add(-1 * time.Second)
	_, err = routerA.Invoke("agent-b", "echo", map[string]interface{}{"key": "val"}, 5*time.Second)
	if err != nil {
		t.Fatalf("invoking echo: %v", err)
	}

	// Inspect the raw request envelope that agent-b received.
	select {
	case msg := <-received:
		var env types.Envelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			t.Fatalf("unmarshaling captured request envelope: %v", err)
		}

		// Verify all required envelope fields.
		if env.ID == "" {
			t.Error("request envelope ID is empty")
		}
		if env.From != "agent-a" {
			t.Errorf("request envelope From = %q, want %q", env.From, "agent-a")
		}
		if env.To != "agent-b" {
			t.Errorf("request envelope To = %q, want %q", env.To, "agent-b")
		}
		if env.Type != types.MessageTypeCapabilityRequest {
			t.Errorf("request envelope Type = %q, want %q", env.Type, types.MessageTypeCapabilityRequest)
		}
		if env.Timestamp.Before(beforeInvoke) {
			t.Errorf("request envelope Timestamp (%v) is before invocation start (%v)", env.Timestamp, beforeInvoke)
		}
		// CorrelationID is only set on the response, not the request.
		if env.CorrelationID != "" {
			t.Errorf("request envelope CorrelationID should be empty, got %q", env.CorrelationID)
		}

	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for captured request message")
	}
}

// ---------------------------------------------------------------------------
// Tool generation
// ---------------------------------------------------------------------------

func TestGenerateTools(t *testing.T) {
	agents := map[string]*types.AgentManifest{
		"lead-agent": {
			Metadata: types.AgentMetadata{
				ID:   "lead-agent",
				Team: "team-alpha",
			},
			Spec: types.AgentSpec{
				Mode: "lead",
			},
		},
		"worker-1": {
			Metadata: types.AgentMetadata{
				ID:   "worker-1",
				Team: "team-alpha",
			},
			Spec: types.AgentSpec{
				Mode: "tool",
				Capabilities: []types.AgentCapability{
					{
						Name:        "search",
						Description: "Search the knowledge base",
						Inputs: []types.CapabilityParam{
							{Name: "query", Type: "string", Description: "Search query", Required: boolPtr(true)},
							{Name: "limit", Type: "integer", Description: "Max results", Required: boolPtr(false)},
						},
						Outputs: []types.CapabilityParam{
							{Name: "results", Type: "array", Description: "Search results"},
						},
					},
				},
			},
		},
		"worker-2": {
			Metadata: types.AgentMetadata{
				ID:   "worker-2",
				Team: "team-alpha",
			},
			Spec: types.AgentSpec{
				Mode: "tool",
				Capabilities: []types.AgentCapability{
					{
						Name:        "summarize",
						Description: "Summarize a document",
						Inputs: []types.CapabilityParam{
							{Name: "document", Type: "string", Description: "Document text"},
						},
						Outputs: []types.CapabilityParam{
							{Name: "summary", Type: "string", Description: "Summary text"},
						},
					},
				},
			},
		},
	}

	tools := GenerateTools(agents, "team-alpha")

	// Should have 2 tools (one per worker capability), lead agent excluded.
	if len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(tools))
	}

	// Tools should be sorted deterministically by agent ID.
	// worker-1 comes before worker-2.
	if tools[0].Name != "worker-1-search" {
		t.Errorf("tools[0].Name = %q, want %q", tools[0].Name, "worker-1-search")
	}
	if tools[0].Description != "Search the knowledge base" {
		t.Errorf("tools[0].Description = %q, want %q", tools[0].Description, "Search the knowledge base")
	}
	if tools[0].AgentID != "worker-1" {
		t.Errorf("tools[0].AgentID = %q, want %q", tools[0].AgentID, "worker-1")
	}

	// Verify inputs for worker-1's search capability.
	if len(tools[0].Inputs) != 2 {
		t.Fatalf("tools[0] expected 2 inputs, got %d", len(tools[0].Inputs))
	}
	if tools[0].Inputs[0].Name != "query" {
		t.Errorf("tools[0].Inputs[0].Name = %q, want %q", tools[0].Inputs[0].Name, "query")
	}
	if tools[0].Inputs[0].Type != "string" {
		t.Errorf("tools[0].Inputs[0].Type = %q, want %q", tools[0].Inputs[0].Type, "string")
	}
	if !tools[0].Inputs[0].Required {
		t.Error("tools[0].Inputs[0].Required should be true")
	}
	if tools[0].Inputs[1].Name != "limit" {
		t.Errorf("tools[0].Inputs[1].Name = %q, want %q", tools[0].Inputs[1].Name, "limit")
	}
	if tools[0].Inputs[1].Required {
		t.Error("tools[0].Inputs[1].Required should be false")
	}

	// Verify outputs.
	if len(tools[0].Outputs) != 1 {
		t.Fatalf("tools[0] expected 1 output, got %d", len(tools[0].Outputs))
	}
	if tools[0].Outputs[0].Name != "results" {
		t.Errorf("tools[0].Outputs[0].Name = %q, want %q", tools[0].Outputs[0].Name, "results")
	}

	// Verify worker-2's tool.
	if tools[1].Name != "worker-2-summarize" {
		t.Errorf("tools[1].Name = %q, want %q", tools[1].Name, "worker-2-summarize")
	}
	if tools[1].AgentID != "worker-2" {
		t.Errorf("tools[1].AgentID = %q, want %q", tools[1].AgentID, "worker-2")
	}
	if len(tools[1].Inputs) != 1 {
		t.Fatalf("tools[1] expected 1 input, got %d", len(tools[1].Inputs))
	}
	// IsRequired() defaults to true when Required pointer is nil.
	if !tools[1].Inputs[0].Required {
		t.Error("tools[1].Inputs[0].Required should default to true when Required is nil")
	}
}

// ---------------------------------------------------------------------------
// Tool file writing
// ---------------------------------------------------------------------------

func TestWriteToolFiles(t *testing.T) {
	tools := []ToolDefinition{
		{
			Name:        "worker-1-search",
			Description: "Search the knowledge base",
			AgentID:     "worker-1",
			Inputs: []ToolParam{
				{Name: "query", Type: "string", Description: "Search query", Required: true},
			},
			Outputs: []ToolParam{
				{Name: "results", Type: "array", Description: "Search results", Required: true},
			},
		},
		{
			Name:        "worker-2-summarize",
			Description: "Summarize a document",
			AgentID:     "worker-2",
			Inputs: []ToolParam{
				{Name: "document", Type: "string", Description: "Document text", Required: true},
			},
			Outputs: []ToolParam{
				{Name: "summary", Type: "string", Description: "Summary text", Required: true},
			},
		},
	}

	dir := filepath.Join(t.TempDir(), "tools")

	if err := WriteToolFiles(tools, dir); err != nil {
		t.Fatalf("WriteToolFiles: %v", err)
	}

	// Verify both files exist and contain valid JSON.
	for _, tool := range tools {
		filePath := filepath.Join(dir, tool.Name+".json")
		data, err := os.ReadFile(filePath)
		if err != nil {
			t.Fatalf("reading tool file %s: %v", filePath, err)
		}

		var got ToolDefinition
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatalf("unmarshaling tool file %s: %v", filePath, err)
		}

		if got.Name != tool.Name {
			t.Errorf("file %s: name = %q, want %q", filePath, got.Name, tool.Name)
		}
		if got.Description != tool.Description {
			t.Errorf("file %s: description = %q, want %q", filePath, got.Description, tool.Description)
		}
		if got.AgentID != tool.AgentID {
			t.Errorf("file %s: agent_id = %q, want %q", filePath, got.AgentID, tool.AgentID)
		}
		if len(got.Inputs) != len(tool.Inputs) {
			t.Errorf("file %s: inputs count = %d, want %d", filePath, len(got.Inputs), len(tool.Inputs))
		}
		if len(got.Outputs) != len(tool.Outputs) {
			t.Errorf("file %s: outputs count = %d, want %d", filePath, len(got.Outputs), len(tool.Outputs))
		}
	}
}
