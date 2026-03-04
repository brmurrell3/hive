// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package events

import (
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/brmurrell3/hive/internal/testutil"
	"github.com/nats-io/nats.go"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// subscribeEvent subscribes to a NATS subject and returns a channel that
// receives the first decoded Event published to that subject.
func subscribeEvent(t *testing.T, nc *nats.Conn, subject string) <-chan Event {
	t.Helper()

	ch := make(chan Event, 1)
	sub, err := nc.Subscribe(subject, func(msg *nats.Msg) {
		var evt Event
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			t.Errorf("unmarshaling event on subject %q: %v", subject, err)
			return
		}
		select {
		case ch <- evt:
		default:
			// Drop additional messages; we only care about the first.
		}
	})
	if err != nil {
		t.Fatalf("subscribing to %q: %v", subject, err)
	}
	t.Cleanup(func() { sub.Unsubscribe() })

	// Flush to ensure the subscription is registered before publish.
	if err := nc.Flush(); err != nil {
		t.Fatalf("flushing after subscribe on %q: %v", subject, err)
	}

	return ch
}

// receiveEvent waits up to 2 seconds for an event on the channel.
func receiveEvent(t *testing.T, ch <-chan Event) Event {
	t.Helper()

	select {
	case evt := <-ch:
		return evt
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for event")
		return Event{} // unreachable
	}
}

// ---------------------------------------------------------------------------
// NewPublisher
// ---------------------------------------------------------------------------

func TestNewPublisher_ReturnsNonNil(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "test-source", testLogger())
	if p == nil {
		t.Fatal("NewPublisher returned nil")
	}
}

func TestNewPublisher_StoresFields(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "my-source", testLogger())
	if p.source != "my-source" {
		t.Errorf("source = %q, want %q", p.source, "my-source")
	}
	if p.nc != nc {
		t.Error("nc field does not match the provided connection")
	}
}

// ---------------------------------------------------------------------------
// Publish — common Event fields
// ---------------------------------------------------------------------------

func TestPublish_EventStructure(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())

	ch := subscribeEvent(t, nc, AgentStarted)

	before := time.Now().UTC().Add(-time.Second)
	p.AgentStarted("agent-123")

	evt := receiveEvent(t, ch)
	after := time.Now().UTC().Add(time.Second)

	// Type and Subject should equal the NATS subject.
	if evt.Type != AgentStarted {
		t.Errorf("Type = %q, want %q", evt.Type, AgentStarted)
	}
	if evt.Subject != AgentStarted {
		t.Errorf("Subject = %q, want %q", evt.Subject, AgentStarted)
	}

	// Source should match what was passed to NewPublisher.
	if evt.Source != "hived" {
		t.Errorf("Source = %q, want %q", evt.Source, "hided")
	}

	// Timestamp should be between before and after.
	if evt.Timestamp.Before(before) || evt.Timestamp.After(after) {
		t.Errorf("Timestamp %v is outside expected window [%v, %v]", evt.Timestamp, before, after)
	}

	// Data should not be nil.
	if evt.Data == nil {
		t.Error("Data is nil, expected non-nil map")
	}
}

func TestPublish_CustomSubject_EventStructure(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "custom-source", testLogger())

	const subject = "hive.events.custom.test"
	ch := subscribeEvent(t, nc, subject)

	p.Publish(subject, map[string]interface{}{"key": "value"})

	evt := receiveEvent(t, ch)

	if evt.Type != subject {
		t.Errorf("Type = %q, want %q", evt.Type, subject)
	}
	if evt.Subject != subject {
		t.Errorf("Subject = %q, want %q", evt.Subject, subject)
	}
	if evt.Source != "custom-source" {
		t.Errorf("Source = %q, want %q", evt.Source, "custom-source")
	}
	if evt.Data["key"] != "value" {
		t.Errorf("Data[key] = %v, want %q", evt.Data["key"], "value")
	}
}

func TestPublish_NilData_EventDelivered(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "src", testLogger())

	const subject = "hive.events.nil.data"
	ch := subscribeEvent(t, nc, subject)

	p.Publish(subject, nil)

	evt := receiveEvent(t, ch)

	if evt.Subject != subject {
		t.Errorf("Subject = %q, want %q", evt.Subject, subject)
	}
	// Data should be nil or empty when nothing was provided.
	if len(evt.Data) != 0 {
		t.Errorf("expected empty/nil Data, got %v", evt.Data)
	}
}

// ---------------------------------------------------------------------------
// AgentCreated
// ---------------------------------------------------------------------------

func TestAgentCreated_Subject(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, AgentCreated)

	p.AgentCreated("agent-abc", "team-alpha")

	evt := receiveEvent(t, ch)

	if evt.Subject != AgentCreated {
		t.Errorf("Subject = %q, want %q", evt.Subject, AgentCreated)
	}
}

func TestAgentCreated_Payload(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, AgentCreated)

	p.AgentCreated("agent-abc", "team-alpha")

	evt := receiveEvent(t, ch)

	if evt.Data["agent_id"] != "agent-abc" {
		t.Errorf("agent_id = %v, want %q", evt.Data["agent_id"], "agent-abc")
	}
	if evt.Data["team"] != "team-alpha" {
		t.Errorf("team = %v, want %q", evt.Data["team"], "team-alpha")
	}
}

// ---------------------------------------------------------------------------
// AgentStarted
// ---------------------------------------------------------------------------

func TestAgentStarted_Subject(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, AgentStarted)

	p.AgentStarted("agent-xyz")

	evt := receiveEvent(t, ch)

	if evt.Subject != AgentStarted {
		t.Errorf("Subject = %q, want %q", evt.Subject, AgentStarted)
	}
	if evt.Type != AgentStarted {
		t.Errorf("Type = %q, want %q", evt.Type, AgentStarted)
	}
}

func TestAgentStarted_Payload(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, AgentStarted)

	p.AgentStarted("agent-xyz")

	evt := receiveEvent(t, ch)

	if evt.Data["agent_id"] != "agent-xyz" {
		t.Errorf("agent_id = %v, want %q", evt.Data["agent_id"], "agent-xyz")
	}
	// AgentStarted must not include unexpected fields like "team" or "reason".
	if _, ok := evt.Data["team"]; ok {
		t.Error("AgentStarted payload should not contain 'team' field")
	}
	if _, ok := evt.Data["reason"]; ok {
		t.Error("AgentStarted payload should not contain 'reason' field")
	}
}

// ---------------------------------------------------------------------------
// AgentStopped
// ---------------------------------------------------------------------------

func TestAgentStopped_Subject(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, AgentStopped)

	p.AgentStopped("agent-stop-1")

	evt := receiveEvent(t, ch)

	if evt.Subject != AgentStopped {
		t.Errorf("Subject = %q, want %q", evt.Subject, AgentStopped)
	}
}

func TestAgentStopped_Payload(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, AgentStopped)

	p.AgentStopped("agent-stop-1")

	evt := receiveEvent(t, ch)

	if evt.Data["agent_id"] != "agent-stop-1" {
		t.Errorf("agent_id = %v, want %q", evt.Data["agent_id"], "agent-stop-1")
	}
}

// ---------------------------------------------------------------------------
// AgentFailed
// ---------------------------------------------------------------------------

func TestAgentFailed_Subject(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, AgentFailed)

	p.AgentFailed("agent-fail-1", "OOM killed")

	evt := receiveEvent(t, ch)

	if evt.Subject != AgentFailed {
		t.Errorf("Subject = %q, want %q", evt.Subject, AgentFailed)
	}
}

func TestAgentFailed_Payload(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, AgentFailed)

	p.AgentFailed("agent-fail-1", "OOM killed")

	evt := receiveEvent(t, ch)

	if evt.Data["agent_id"] != "agent-fail-1" {
		t.Errorf("agent_id = %v, want %q", evt.Data["agent_id"], "agent-fail-1")
	}
	if evt.Data["reason"] != "OOM killed" {
		t.Errorf("reason = %v, want %q", evt.Data["reason"], "OOM killed")
	}
}

func TestAgentFailed_EmptyReason(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, AgentFailed)

	p.AgentFailed("agent-fail-2", "")

	evt := receiveEvent(t, ch)

	if evt.Data["agent_id"] != "agent-fail-2" {
		t.Errorf("agent_id = %v, want %q", evt.Data["agent_id"], "agent-fail-2")
	}
	if evt.Data["reason"] != "" {
		t.Errorf("reason = %v, want empty string", evt.Data["reason"])
	}
}

// ---------------------------------------------------------------------------
// CapabilityRegistered
// ---------------------------------------------------------------------------

func TestCapabilityRegistered_Subject(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, CapabilityRegistered)

	p.CapabilityRegistered("agent-1", "search")

	evt := receiveEvent(t, ch)

	if evt.Subject != CapabilityRegistered {
		t.Errorf("Subject = %q, want %q", evt.Subject, CapabilityRegistered)
	}
}

func TestCapabilityRegistered_Payload(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, CapabilityRegistered)

	p.CapabilityRegistered("agent-1", "search")

	evt := receiveEvent(t, ch)

	if evt.Data["agent_id"] != "agent-1" {
		t.Errorf("agent_id = %v, want %q", evt.Data["agent_id"], "agent-1")
	}
	if evt.Data["capability"] != "search" {
		t.Errorf("capability = %v, want %q", evt.Data["capability"], "search")
	}
}

// ---------------------------------------------------------------------------
// CapabilityInvoked
// ---------------------------------------------------------------------------

func TestCapabilityInvoked_Subject(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, CapabilityInvoked)

	p.CapabilityInvoked("agent-a", "agent-b", "summarise")

	evt := receiveEvent(t, ch)

	if evt.Subject != CapabilityInvoked {
		t.Errorf("Subject = %q, want %q", evt.Subject, CapabilityInvoked)
	}
}

func TestCapabilityInvoked_Payload(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, CapabilityInvoked)

	p.CapabilityInvoked("agent-a", "agent-b", "summarise")

	evt := receiveEvent(t, ch)

	if evt.Data["from"] != "agent-a" {
		t.Errorf("from = %v, want %q", evt.Data["from"], "agent-a")
	}
	if evt.Data["to"] != "agent-b" {
		t.Errorf("to = %v, want %q", evt.Data["to"], "agent-b")
	}
	if evt.Data["capability"] != "summarise" {
		t.Errorf("capability = %v, want %q", evt.Data["capability"], "summarise")
	}
}

// ---------------------------------------------------------------------------
// NodeJoined
// ---------------------------------------------------------------------------

func TestNodeJoined_Subject(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, NodeJoined)

	p.NodeJoined("node-42")

	evt := receiveEvent(t, ch)

	if evt.Subject != NodeJoined {
		t.Errorf("Subject = %q, want %q", evt.Subject, NodeJoined)
	}
}

func TestNodeJoined_Payload(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, NodeJoined)

	p.NodeJoined("node-42")

	evt := receiveEvent(t, ch)

	if evt.Data["node_id"] != "node-42" {
		t.Errorf("node_id = %v, want %q", evt.Data["node_id"], "node-42")
	}
}

// ---------------------------------------------------------------------------
// NodeLeft
// ---------------------------------------------------------------------------

func TestNodeLeft_Subject(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, NodeLeft)

	p.NodeLeft("node-99")

	evt := receiveEvent(t, ch)

	if evt.Subject != NodeLeft {
		t.Errorf("Subject = %q, want %q", evt.Subject, NodeLeft)
	}
}

func TestNodeLeft_Payload(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, NodeLeft)

	p.NodeLeft("node-99")

	evt := receiveEvent(t, ch)

	if evt.Data["node_id"] != "node-99" {
		t.Errorf("node_id = %v, want %q", evt.Data["node_id"], "node-99")
	}
}

// ---------------------------------------------------------------------------
// Subject constant values
// ---------------------------------------------------------------------------

func TestSubjectConstants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		constant string
		want     string
	}{
		{AgentCreated, "hive.events.agent.created"},
		{AgentStarted, "hive.events.agent.started"},
		{AgentStopped, "hive.events.agent.stopped"},
		{AgentFailed, "hive.events.agent.failed"},
		{CapabilityRegistered, "hive.events.capability.registered"},
		{CapabilityInvoked, "hive.events.capability.invoked"},
		{NodeJoined, "hive.events.node.joined"},
		{NodeLeft, "hive.events.node.left"},
	}

	for _, tt := range tests {
		if tt.constant != tt.want {
			t.Errorf("constant value = %q, want %q", tt.constant, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Multiple events on the same publisher
// ---------------------------------------------------------------------------

func TestPublisher_MultipleEventsOnDifferentSubjects(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())

	chStarted := subscribeEvent(t, nc, AgentStarted)
	chStopped := subscribeEvent(t, nc, AgentStopped)
	chFailed := subscribeEvent(t, nc, AgentFailed)

	p.AgentStarted("agent-1")
	p.AgentStopped("agent-2")
	p.AgentFailed("agent-3", "crash")

	evtStarted := receiveEvent(t, chStarted)
	evtStopped := receiveEvent(t, chStopped)
	evtFailed := receiveEvent(t, chFailed)

	if evtStarted.Data["agent_id"] != "agent-1" {
		t.Errorf("AgentStarted agent_id = %v, want %q", evtStarted.Data["agent_id"], "agent-1")
	}
	if evtStopped.Data["agent_id"] != "agent-2" {
		t.Errorf("AgentStopped agent_id = %v, want %q", evtStopped.Data["agent_id"], "agent-2")
	}
	if evtFailed.Data["agent_id"] != "agent-3" {
		t.Errorf("AgentFailed agent_id = %v, want %q", evtFailed.Data["agent_id"], "agent-3")
	}
	if evtFailed.Data["reason"] != "crash" {
		t.Errorf("AgentFailed reason = %v, want %q", evtFailed.Data["reason"], "crash")
	}
}

// ---------------------------------------------------------------------------
// Raw JSON round-trip
// ---------------------------------------------------------------------------

func TestPublish_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())

	rawCh := make(chan []byte, 1)
	sub, err := nc.Subscribe(NodeJoined, func(msg *nats.Msg) {
		cp := make([]byte, len(msg.Data))
		copy(cp, msg.Data)
		select {
		case rawCh <- cp:
		default:
		}
	})
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	defer sub.Unsubscribe()

	if err := nc.Flush(); err != nil {
		t.Fatalf("flushing: %v", err)
	}

	p.NodeJoined("node-round-trip")

	var raw []byte
	select {
	case raw = <-rawCh:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for raw message")
	}

	// The payload must be valid JSON that decodes into an Event.
	var evt Event
	if err := json.Unmarshal(raw, &evt); err != nil {
		t.Fatalf("raw payload is not valid JSON: %v\npayload: %s", err, string(raw))
	}

	// Verify required top-level JSON keys are present and populated.
	var raw2 map[string]interface{}
	if err := json.Unmarshal(raw, &raw2); err != nil {
		t.Fatalf("second unmarshal: %v", err)
	}

	for _, key := range []string{"type", "timestamp", "source", "subject"} {
		if _, ok := raw2[key]; !ok {
			t.Errorf("JSON payload missing top-level key %q", key)
		}
	}

	if raw2["type"] != NodeJoined {
		t.Errorf("JSON type = %v, want %q", raw2["type"], NodeJoined)
	}
	if raw2["source"] != "hived" {
		t.Errorf("JSON source = %v, want %q", raw2["source"], "hived")
	}
	if raw2["subject"] != NodeJoined {
		t.Errorf("JSON subject = %v, want %q", raw2["subject"], NodeJoined)
	}
}

// ---------------------------------------------------------------------------
// Source propagation across all typed methods
// ---------------------------------------------------------------------------

func TestPublisher_SourcePropagatedInAllEvents(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	const source = "agent-manager"
	p := NewPublisher(nc, source, testLogger())

	type testCase struct {
		name    string
		subject string
		publish func()
	}

	cases := []testCase{
		{
			name:    "AgentCreated",
			subject: AgentCreated,
			publish: func() { p.AgentCreated("a", "t") },
		},
		{
			name:    "AgentStarted",
			subject: AgentStarted,
			publish: func() { p.AgentStarted("a") },
		},
		{
			name:    "AgentStopped",
			subject: AgentStopped,
			publish: func() { p.AgentStopped("a") },
		},
		{
			name:    "AgentFailed",
			subject: AgentFailed,
			publish: func() { p.AgentFailed("a", "r") },
		},
		{
			name:    "CapabilityRegistered",
			subject: CapabilityRegistered,
			publish: func() { p.CapabilityRegistered("a", "cap") },
		},
		{
			name:    "CapabilityInvoked",
			subject: CapabilityInvoked,
			publish: func() { p.CapabilityInvoked("a", "b", "cap") },
		},
		{
			name:    "NodeJoined",
			subject: NodeJoined,
			publish: func() { p.NodeJoined("n") },
		},
		{
			name:    "NodeLeft",
			subject: NodeLeft,
			publish: func() { p.NodeLeft("n") },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ch := subscribeEvent(t, nc, tc.subject)
			tc.publish()
			evt := receiveEvent(t, ch)
			if evt.Source != source {
				t.Errorf("Source = %q, want %q", evt.Source, source)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Timestamp is in UTC
// ---------------------------------------------------------------------------

func TestPublish_TimestampIsUTC(t *testing.T) {
	t.Parallel()
	srv := testutil.NATSServer(t)
	nc := testutil.NATSConnect(t, srv)

	p := NewPublisher(nc, "hived", testLogger())
	ch := subscribeEvent(t, nc, AgentStarted)

	p.AgentStarted("agent-ts")

	evt := receiveEvent(t, ch)

	if evt.Timestamp.Location() != time.UTC {
		t.Errorf("Timestamp location = %v, want UTC", evt.Timestamp.Location())
	}
	if evt.Timestamp.IsZero() {
		t.Error("Timestamp is zero")
	}
}
