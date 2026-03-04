//go:build integration

package mqtt

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/testutil"
	"github.com/hivehq/hive/internal/types"
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
	path := filepath.Join(dir, "state.json")
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

// buildMQTTConnect builds an MQTT CONNECT packet.
// clientID is the MQTT client identifier.
// username and password are optional MQTT credentials.
func buildMQTTConnect(clientID, username, password string) []byte {
	// Variable header
	var varHeader []byte
	// Protocol Name: "MQTT"
	varHeader = append(varHeader, 0x00, 0x04) // length
	varHeader = append(varHeader, []byte("MQTT")...)
	// Protocol Level: 4 (MQTT 3.1.1)
	varHeader = append(varHeader, 0x04)
	// Connect Flags
	flags := byte(0x02) // clean session
	if username != "" {
		flags |= 0x80 // username flag
	}
	if password != "" {
		flags |= 0x40 // password flag
	}
	varHeader = append(varHeader, flags)
	// Keep Alive: 60s
	varHeader = append(varHeader, 0x00, 0x3C)

	// Payload
	var payload []byte
	// Client ID
	payload = append(payload, mqttString(clientID)...)
	if username != "" {
		payload = append(payload, mqttString(username)...)
	}
	if password != "" {
		payload = append(payload, mqttString(password)...)
	}

	remainLen := len(varHeader) + len(payload)

	var packet []byte
	packet = append(packet, 0x10) // CONNECT packet type
	packet = append(packet, encodeRemainingLength(remainLen)...)
	packet = append(packet, varHeader...)
	packet = append(packet, payload...)

	return packet
}

// buildMQTTPublish builds an MQTT PUBLISH packet (QoS 0).
func buildMQTTPublish(topic string, payload []byte) []byte {
	topicBytes := []byte(topic)
	remainLen := 2 + len(topicBytes) + len(payload)

	var packet []byte
	packet = append(packet, 0x30) // PUBLISH, QoS 0
	packet = append(packet, encodeRemainingLength(remainLen)...)
	// Topic length (2 bytes, big-endian)
	packet = append(packet, byte(len(topicBytes)>>8), byte(len(topicBytes)))
	packet = append(packet, topicBytes...)
	packet = append(packet, payload...)

	return packet
}

// mqttString encodes a string as an MQTT length-prefixed UTF-8 string.
func mqttString(s string) []byte {
	b := make([]byte, 2+len(s))
	binary.BigEndian.PutUint16(b, uint16(len(s)))
	copy(b[2:], s)
	return b
}

// startBridge creates and starts an MQTT bridge for testing, using port 0 for random allocation.
func startBridge(t *testing.T, nc *nats.Conn, store *state.Store) *Bridge {
	t.Helper()

	bridge := NewBridge(Config{
		Port:     0, // random port
		NATSConn: nc,
		Store:    store,
		Logger:   testLogger(t),
	})

	if err := bridge.Start(); err != nil {
		t.Fatalf("bridge.Start: %v", err)
	}

	t.Cleanup(func() {
		bridge.Stop()
	})

	return bridge
}

// connectAndAuth creates a TCP connection to the bridge and sends an MQTT CONNECT.
// Returns the connection and the CONNACK return code.
func connectAndAuth(t *testing.T, bridge *Bridge, clientID, username, password string) (net.Conn, byte) {
	t.Helper()

	addr := fmt.Sprintf("127.0.0.1:%d", bridge.Port())
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("TCP connect to bridge: %v", err)
	}

	// Send CONNECT
	pkt := buildMQTTConnect(clientID, username, password)
	if _, err := conn.Write(pkt); err != nil {
		conn.Close()
		t.Fatalf("sending CONNECT: %v", err)
	}

	// Read CONNACK (4 bytes: 0x20, 0x02, session_present, return_code)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4)
	n, err := conn.Read(buf)
	if err != nil {
		conn.Close()
		t.Fatalf("reading CONNACK: %v", err)
	}
	if n < 4 {
		conn.Close()
		t.Fatalf("CONNACK too short: got %d bytes", n)
	}
	if buf[0] != 0x20 {
		conn.Close()
		t.Fatalf("expected CONNACK (0x20), got 0x%02X", buf[0])
	}

	returnCode := buf[3]
	if returnCode != 0 {
		conn.Close()
	}

	return conn, returnCode
}

// ---------------------------------------------------------------------------
// MQTT bridge starts and accepts TCP connections
// ---------------------------------------------------------------------------

func TestBridge_StartsAndAcceptsTCP(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	bridge := startBridge(t, nc, store)

	port := bridge.Port()
	if port == 0 {
		t.Fatal("bridge port is 0, expected a real port")
	}

	// Verify we can open a TCP connection.
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to bridge at %s: %v", addr, err)
	}
	conn.Close()
}

// ---------------------------------------------------------------------------
// MQTT CONNECT packet handling with auth
// ---------------------------------------------------------------------------

func TestBridge_ConnectWithValidAuth(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)
	rawToken := addTestToken(t, store)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	bridge := startBridge(t, nc, store)

	conn, returnCode := connectAndAuth(t, bridge, "firmware-01", "firmware-01", rawToken)
	defer conn.Close()

	if returnCode != 0 {
		t.Errorf("CONNACK return code = %d, want 0 (success)", returnCode)
	}

	// Verify the client is tracked.
	// Allow a small window for the bridge to register the client.
	time.Sleep(50 * time.Millisecond)
	agents := bridge.ConnectedAgents()
	found := false
	for _, a := range agents {
		if a == "firmware-01" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected firmware-01 in connected agents, got %v", agents)
	}
}

func TestBridge_ConnectWithInvalidAuth(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)
	addTestToken(t, store)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	bridge := startBridge(t, nc, store)

	_, returnCode := connectAndAuth(t, bridge, "bad-client", "bad-client", "wrong-token-value")

	// Return code 5 = not authorized
	if returnCode != 5 {
		t.Errorf("CONNACK return code = %d, want 5 (not authorized)", returnCode)
	}
}

func TestBridge_ConnectWithNoPassword(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	bridge := startBridge(t, nc, store)

	// T3-03: CONNECT with no password should be rejected (auth required).
	conn, returnCode := connectAndAuth(t, bridge, "anon-client", "", "")
	defer conn.Close()

	if returnCode != 5 {
		t.Errorf("CONNACK return code = %d, want 5 (not authorized with no password)", returnCode)
	}
}

// ---------------------------------------------------------------------------
// MQTT PUBLISH bridges to NATS
// ---------------------------------------------------------------------------

func TestBridge_PublishBridgesToNATS(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)
	rawToken := addTestToken(t, store)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	bridge := startBridge(t, nc, store)

	// Subscribe on NATS for the expected subject.
	received := make(chan []byte, 1)
	sub, err := nc.Subscribe("hive.health.sensor-01", func(msg *nats.Msg) {
		received <- msg.Data
	})
	if err != nil {
		t.Fatalf("NATS subscribe: %v", err)
	}
	defer sub.Unsubscribe()
	nc.Flush()

	// Connect to bridge as an MQTT client.
	conn, returnCode := connectAndAuth(t, bridge, "sensor-01", "sensor-01", rawToken)
	if returnCode != 0 {
		t.Fatalf("CONNACK return code = %d, want 0", returnCode)
	}
	defer conn.Close()

	// Publish via MQTT.
	payload := []byte(`{"status":"alive","uptime":1234}`)
	pubPkt := buildMQTTPublish("hive/health/sensor-01", payload)
	if _, err := conn.Write(pubPkt); err != nil {
		t.Fatalf("sending PUBLISH: %v", err)
	}

	// Wait for the message to arrive on NATS.
	select {
	case data := <-received:
		if string(data) != string(payload) {
			t.Errorf("received payload = %q, want %q", string(data), string(payload))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for NATS message from MQTT publish")
	}
}

// ---------------------------------------------------------------------------
// Topic-to-subject mapping
// ---------------------------------------------------------------------------

func TestTopicToSubjectMapping(t *testing.T) {
	tests := []struct {
		name        string
		mqttTopic   string
		natsSubject string
	}{
		{
			name:        "health topic",
			mqttTopic:   "hive/health/agent-1",
			natsSubject: "hive.health.agent-1",
		},
		{
			name:        "agent inbox",
			mqttTopic:   "hive/agent/agent-1/inbox",
			natsSubject: "hive.agent.agent-1.inbox",
		},
		{
			name:        "team broadcast",
			mqttTopic:   "hive/team/sensors/broadcast",
			natsSubject: "hive.team.sensors.broadcast",
		},
		{
			name:        "capability request",
			mqttTopic:   "hive/capabilities/agent-1/read-temp/request",
			natsSubject: "hive.capabilities.agent-1.read-temp.request",
		},
		{
			name:        "capability response",
			mqttTopic:   "hive/capabilities/agent-1/read-temp/response",
			natsSubject: "hive.capabilities.agent-1.read-temp.response",
		},
		{
			name:        "join request",
			mqttTopic:   "hive/join/request",
			natsSubject: "hive.join.request",
		},
		{
			name:        "join status",
			mqttTopic:   "hive/join/status/agent-1",
			natsSubject: "hive.join.status.agent-1",
		},
		{
			name:        "single level",
			mqttTopic:   "test",
			natsSubject: "test",
		},
		{
			name:        "deeply nested",
			mqttTopic:   "a/b/c/d/e/f",
			natsSubject: "a.b.c.d.e.f",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mqttTopicToNATSSubject(tt.mqttTopic)
			if got != tt.natsSubject {
				t.Errorf("mqttTopicToNATSSubject(%q) = %q, want %q", tt.mqttTopic, got, tt.natsSubject)
			}

			// Also test the reverse mapping.
			gotReverse := natsSubjectToMQTTTopic(tt.natsSubject)
			if gotReverse != tt.mqttTopic {
				t.Errorf("natsSubjectToMQTTTopic(%q) = %q, want %q", tt.natsSubject, gotReverse, tt.mqttTopic)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Bridge Stop is clean
// ---------------------------------------------------------------------------

func TestBridge_StopIsClean(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	bridge := NewBridge(Config{
		Port:     0,
		NATSConn: nc,
		Store:    store,
		Logger:   testLogger(t),
	})

	if err := bridge.Start(); err != nil {
		t.Fatalf("bridge.Start: %v", err)
	}

	port := bridge.Port()

	// Stop the bridge.
	if err := bridge.Stop(); err != nil {
		t.Fatalf("bridge.Stop: %v", err)
	}

	// Verify the port is no longer accepting connections.
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Error("expected connection to be refused after bridge stop")
	}
}

// ---------------------------------------------------------------------------
// MQTT PINGREQ handling
// ---------------------------------------------------------------------------

func TestBridge_PingPong(t *testing.T) {
	srv := testutil.NATSServer(t)
	store := testStateStore(t)
	rawToken := addTestToken(t, store)

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		t.Fatalf("NATS connect: %v", err)
	}
	defer nc.Close()

	bridge := startBridge(t, nc, store)

	// Connect and authenticate.
	conn, returnCode := connectAndAuth(t, bridge, "ping-client", "ping-client", rawToken)
	if returnCode != 0 {
		t.Fatalf("CONNACK return code = %d, want 0", returnCode)
	}
	defer conn.Close()

	// Send PINGREQ.
	pingreq := []byte{0xC0, 0x00}
	if _, err := conn.Write(pingreq); err != nil {
		t.Fatalf("sending PINGREQ: %v", err)
	}

	// Read PINGRESP (0xD0, 0x00).
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 2)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("reading PINGRESP: %v", err)
	}
	if n < 2 {
		t.Fatalf("PINGRESP too short: %d bytes", n)
	}
	if buf[0] != 0xD0 || buf[1] != 0x00 {
		t.Errorf("PINGRESP = [0x%02X, 0x%02X], want [0xD0, 0x00]", buf[0], buf[1])
	}
}
