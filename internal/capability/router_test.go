//go:build integration

package capability

import (
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	hivenats "github.com/brmurrell3/hive/internal/nats"
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

func testNATSConn(t *testing.T, srv *hivenats.Server) *nats.Conn {
	t.Helper()
	nc, err := nats.Connect(srv.ClientURL(), nats.Token(srv.AuthToken()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	return nc
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
