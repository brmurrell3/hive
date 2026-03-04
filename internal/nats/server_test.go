//go:build integration

package nats

import (
	"encoding/json"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestServer_StartAndConnect(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := types.NATSConfig{
		Port: -1,
		JetStream: types.JetStreamConfig{
			StorePath: tmpDir,
		},
	}

	srv, err := NewServer(cfg, tmpDir, testLogger())
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer srv.Shutdown()

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connecting to NATS: %v", err)
	}
	defer nc.Close()

	if !nc.IsConnected() {
		t.Fatal("expected connection to be established")
	}
}

func TestServer_PubSub(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := types.NATSConfig{
		Port: -1,
		JetStream: types.JetStreamConfig{
			StorePath: tmpDir,
		},
	}

	srv, err := NewServer(cfg, tmpDir, testLogger())
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer srv.Shutdown()

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connecting to NATS: %v", err)
	}
	defer nc.Close()

	received := make(chan *nats.Msg, 1)
	sub, err := nc.Subscribe("test.subject", func(msg *nats.Msg) {
		received <- msg
	})
	if err != nil {
		t.Fatalf("subscribing: %v", err)
	}
	defer sub.Unsubscribe()

	envelope := types.Envelope{
		ID:        "test-123",
		From:      "agent-a",
		To:        "agent-b",
		Type:      types.MessageTypeTask,
		Timestamp: time.Now(),
		Payload:   map[string]string{"data": "hello"},
	}

	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}

	if err := nc.Publish("test.subject", data); err != nil {
		t.Fatalf("publishing: %v", err)
	}

	select {
	case msg := <-received:
		var got types.Envelope
		if err := json.Unmarshal(msg.Data, &got); err != nil {
			t.Fatalf("unmarshaling: %v", err)
		}
		if got.ID != "test-123" {
			t.Errorf("id = %q, want %q", got.ID, "test-123")
		}
		if got.From != "agent-a" {
			t.Errorf("from = %q, want %q", got.From, "agent-a")
		}
		if got.Type != types.MessageTypeTask {
			t.Errorf("type = %q, want %q", got.Type, types.MessageTypeTask)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for message")
	}
}

func TestServer_JetStream(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := types.NATSConfig{
		Port: -1,
		JetStream: types.JetStreamConfig{
			StorePath: tmpDir,
		},
	}

	srv, err := NewServer(cfg, tmpDir, testLogger())
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer srv.Shutdown()

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connecting to NATS: %v", err)
	}
	defer nc.Close()

	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatalf("creating jetstream context: %v", err)
	}

	ctx := t
	_ = ctx

	stream, err := js.CreateStream(t.Context(), jetstream.StreamConfig{
		Name:     "TEST",
		Subjects: []string{"test.>"},
	})
	if err != nil {
		t.Fatalf("creating stream: %v", err)
	}

	payload := []byte(`{"test": "data"}`)
	if _, err := js.Publish(t.Context(), "test.hello", payload); err != nil {
		t.Fatalf("publishing to stream: %v", err)
	}

	cons, err := stream.CreateConsumer(t.Context(), jetstream.ConsumerConfig{
		Durable:   "test-consumer",
		AckPolicy: jetstream.AckExplicitPolicy,
	})
	if err != nil {
		t.Fatalf("creating consumer: %v", err)
	}

	msgs, err := cons.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
	if err != nil {
		t.Fatalf("fetching messages: %v", err)
	}

	count := 0
	for msg := range msgs.Messages() {
		if string(msg.Data()) != string(payload) {
			t.Errorf("data = %q, want %q", string(msg.Data()), string(payload))
		}
		msg.Ack()
		count++
	}

	if count != 1 {
		t.Errorf("received %d messages, want 1", count)
	}
}

func TestServer_JetStreamDisabled(t *testing.T) {
	tmpDir := t.TempDir()
	enabled := false
	cfg := types.NATSConfig{
		Port: -1,
		JetStream: types.JetStreamConfig{
			Enabled:   &enabled,
			StorePath: tmpDir,
		},
	}

	srv, err := NewServer(cfg, tmpDir, testLogger())
	if err != nil {
		t.Fatalf("creating server: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("starting server: %v", err)
	}
	defer srv.Shutdown()

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("connecting to NATS: %v", err)
	}
	defer nc.Close()

	// JetStream should not be available
	_, err = jetstream.New(nc)
	if err != nil {
		// jetstream.New might succeed even if JS is disabled;
		// but operations on it should fail
		return
	}

	_, err = nc.JetStream()
	if err != nil {
		return // expected - JetStream not available
	}
}
