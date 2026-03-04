//go:build integration

package dashboard

import (
	"bufio"
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivehq/hive/internal/auth"
	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// --- Mock Store ---

type mockStore struct {
	mu     sync.RWMutex
	agents []*state.AgentState
	nodes  []*types.NodeState
	caps   *types.CapabilityRegistry
	users  []*auth.User
}

func newMockStore() *mockStore {
	return &mockStore{
		caps: types.NewCapabilityRegistry(),
	}
}

func (m *mockStore) AllAgents() []*state.AgentState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*state.AgentState, len(m.agents))
	copy(result, m.agents)
	return result
}

func (m *mockStore) GetAgent(id string) *state.AgentState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, a := range m.agents {
		if a.ID == id {
			cp := *a
			return &cp
		}
	}
	return nil
}

func (m *mockStore) AllNodes() []*types.NodeState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*types.NodeState, len(m.nodes))
	copy(result, m.nodes)
	return result
}

func (m *mockStore) GetNode(id string) *types.NodeState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, n := range m.nodes {
		if n.ID == id {
			cp := *n
			return &cp
		}
	}
	return nil
}

func (m *mockStore) GetCapabilityRegistry() *types.CapabilityRegistry {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.caps == nil {
		return types.NewCapabilityRegistry()
	}
	// Return a copy.
	reg := types.NewCapabilityRegistry()
	for k, v := range m.caps.Agents {
		cp := *v
		reg.Agents[k] = &cp
	}
	return reg
}

func (m *mockStore) AllUsers() []*auth.User {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]*auth.User, len(m.users))
	copy(result, m.users)
	return result
}

// --- Mock NATS Connection ---

type mockNATSConn struct {
	mu   sync.Mutex
	subs map[string]nats.MsgHandler
}

func newMockNATSConn() *mockNATSConn {
	return &mockNATSConn{
		subs: make(map[string]nats.MsgHandler),
	}
}

func (m *mockNATSConn) Subscribe(subject string, handler nats.MsgHandler) (*nats.Subscription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.subs[subject] = handler
	// Return a nil subscription; tests that need it can handle it.
	// Since we can't create a real *nats.Subscription without a real connection,
	// we return nil. The server only uses the subscription for Unsubscribe on shutdown.
	return nil, nil
}

func (m *mockNATSConn) Publish(subject string, data []byte) error {
	return nil
}

// --- Test Helpers ---

func setupTestServer(t *testing.T) (*httptest.Server, *mockStore) {
	t.Helper()
	store := newMockStore()
	srv := NewServer(Config{
		Store: store,
		Addr:  ":0",
	})
	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(func() { ts.Close() })
	return ts, store
}

func getJSON(t *testing.T, url string) (int, map[string]interface{}) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s failed: %v", url, err)
	}
	defer resp.Body.Close()

	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding JSON response: %v", err)
	}
	return resp.StatusCode, result
}

func getJSONList(t *testing.T, url string) (int, []interface{}) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s failed: %v", url, err)
	}
	defer resp.Body.Close()

	var result []interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decoding JSON list response: %v", err)
	}
	return resp.StatusCode, result
}

// --- Tests ---

func TestServer_ClusterEndpoint(t *testing.T) {
	ts, store := setupTestServer(t)

	// Add some agents to the store.
	store.mu.Lock()
	store.agents = []*state.AgentState{
		{ID: "agent-1", Team: "team-alpha", Status: state.AgentStatusRunning},
		{ID: "agent-2", Team: "team-alpha", Status: state.AgentStatusRunning},
		{ID: "agent-3", Team: "team-beta", Status: state.AgentStatusStopped},
	}
	store.nodes = []*types.NodeState{
		{ID: "node-1", Status: types.NodeStatusOnline},
	}
	store.mu.Unlock()

	status, result := getJSON(t, ts.URL+"/api/cluster")
	if status != http.StatusOK {
		t.Fatalf("expected status 200, got %d", status)
	}

	// Verify counts.
	if nodeCount, ok := result["node_count"].(float64); !ok || int(nodeCount) != 1 {
		t.Errorf("expected node_count=1, got %v", result["node_count"])
	}
	if teamCount, ok := result["team_count"].(float64); !ok || int(teamCount) != 2 {
		t.Errorf("expected team_count=2, got %v", result["team_count"])
	}
	if agentCount, ok := result["agent_count"].(float64); !ok || int(agentCount) != 3 {
		t.Errorf("expected agent_count=3, got %v", result["agent_count"])
	}
	if _, ok := result["uptime_seconds"]; !ok {
		t.Error("expected uptime_seconds in response")
	}
	if _, ok := result["agent_status"]; !ok {
		t.Error("expected agent_status in response")
	}
}

func TestServer_AgentsEndpoint(t *testing.T) {
	ts, store := setupTestServer(t)

	// Add agents.
	store.mu.Lock()
	store.agents = []*state.AgentState{
		{ID: "coder", Team: "dev", Status: state.AgentStatusRunning},
		{ID: "reviewer", Team: "dev", Status: state.AgentStatusStopped},
	}
	store.mu.Unlock()

	status, result := getJSONList(t, ts.URL+"/api/agents")
	if status != http.StatusOK {
		t.Fatalf("expected status 200, got %d", status)
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 agents, got %d", len(result))
	}

	// Verify first agent.
	agent0, ok := result[0].(map[string]interface{})
	if !ok {
		t.Fatal("expected agent to be a JSON object")
	}
	if agent0["id"] != "coder" {
		t.Errorf("expected agent id 'coder', got %v", agent0["id"])
	}
}

func TestServer_AgentDetailEndpoint(t *testing.T) {
	ts, store := setupTestServer(t)

	store.mu.Lock()
	store.agents = []*state.AgentState{
		{ID: "researcher", Team: "science", Status: state.AgentStatusRunning, RestartCount: 2},
	}
	store.mu.Unlock()

	// Test existing agent.
	status, result := getJSON(t, ts.URL+"/api/agents/researcher")
	if status != http.StatusOK {
		t.Fatalf("expected status 200, got %d", status)
	}
	if result["id"] != "researcher" {
		t.Errorf("expected id 'researcher', got %v", result["id"])
	}
	if result["team"] != "science" {
		t.Errorf("expected team 'science', got %v", result["team"])
	}
	if restarts, ok := result["restart_count"].(float64); !ok || int(restarts) != 2 {
		t.Errorf("expected restart_count=2, got %v", result["restart_count"])
	}

	// Test non-existent agent.
	status, errResult := getJSON(t, ts.URL+"/api/agents/nonexistent")
	if status != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", status)
	}
	if _, ok := errResult["error"]; !ok {
		t.Error("expected error message in response")
	}
}

func TestServer_NodesEndpoint(t *testing.T) {
	ts, store := setupTestServer(t)

	store.mu.Lock()
	store.nodes = []*types.NodeState{
		{ID: "node-1", Status: types.NodeStatusOnline, Tier: types.NodeTier1, Arch: "amd64"},
		{ID: "node-2", Status: types.NodeStatusOffline, Tier: types.NodeTier2, Arch: "arm64"},
	}
	store.mu.Unlock()

	status, result := getJSONList(t, ts.URL+"/api/nodes")
	if status != http.StatusOK {
		t.Fatalf("expected status 200, got %d", status)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 nodes, got %d", len(result))
	}
}

func TestServer_NodeDetailEndpoint(t *testing.T) {
	ts, store := setupTestServer(t)

	store.mu.Lock()
	store.nodes = []*types.NodeState{
		{ID: "node-42", Status: types.NodeStatusOnline, Arch: "amd64"},
	}
	store.mu.Unlock()

	// Existing node.
	status, result := getJSON(t, ts.URL+"/api/nodes/node-42")
	if status != http.StatusOK {
		t.Fatalf("expected status 200, got %d", status)
	}
	if result["id"] != "node-42" {
		t.Errorf("expected id 'node-42', got %v", result["id"])
	}

	// Non-existent node.
	status, _ = getJSON(t, ts.URL+"/api/nodes/nonexistent")
	if status != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", status)
	}
}

func TestServer_CapabilitiesEndpoint(t *testing.T) {
	ts, store := setupTestServer(t)

	store.mu.Lock()
	store.caps = types.NewCapabilityRegistry()
	store.caps.Register("agent-1", "team-a", "1", "node-1", []types.AgentCapability{
		{Name: "code-review", Description: "Reviews code"},
		{Name: "testing", Description: "Runs tests"},
	})
	store.caps.Register("agent-2", "team-b", "2", "node-1", []types.AgentCapability{
		{Name: "code-review", Description: "Reviews code"},
	})
	store.mu.Unlock()

	status, result := getJSON(t, ts.URL+"/api/capabilities")
	if status != http.StatusOK {
		t.Fatalf("expected status 200, got %d", status)
	}

	// Verify agents are present.
	agents, ok := result["agents"].(map[string]interface{})
	if !ok {
		t.Fatal("expected 'agents' key in response")
	}
	if len(agents) != 2 {
		t.Errorf("expected 2 agent entries, got %d", len(agents))
	}

	// Verify capabilities index.
	caps, ok := result["capabilities"].(map[string]interface{})
	if !ok {
		t.Fatal("expected 'capabilities' key in response")
	}
	codeReview, ok := caps["code-review"].([]interface{})
	if !ok {
		t.Fatal("expected 'code-review' capability")
	}
	if len(codeReview) != 2 {
		t.Errorf("expected 2 agents providing code-review, got %d", len(codeReview))
	}
}

func TestServer_CapabilitiesEndpoint_Empty(t *testing.T) {
	ts, _ := setupTestServer(t)

	status, result := getJSON(t, ts.URL+"/api/capabilities")
	if status != http.StatusOK {
		t.Fatalf("expected status 200, got %d", status)
	}

	if _, ok := result["agents"]; !ok {
		t.Error("expected 'agents' key in response")
	}
	if _, ok := result["capabilities"]; !ok {
		t.Error("expected 'capabilities' key in response")
	}
}

func TestServer_LogsEndpoint(t *testing.T) {
	ts, store := setupTestServer(t)

	store.mu.Lock()
	store.agents = []*state.AgentState{
		{ID: "logger-agent", Team: "ops", Status: state.AgentStatusRunning},
	}
	store.mu.Unlock()

	// Existing agent.
	status, result := getJSON(t, ts.URL+"/api/logs/logger-agent")
	if status != http.StatusOK {
		t.Fatalf("expected status 200, got %d", status)
	}
	if result["agent_id"] != "logger-agent" {
		t.Errorf("expected agent_id 'logger-agent', got %v", result["agent_id"])
	}

	// Non-existent agent.
	status, _ = getJSON(t, ts.URL+"/api/logs/nonexistent")
	if status != http.StatusNotFound {
		t.Fatalf("expected status 404, got %d", status)
	}
}

func TestServer_WebSocket(t *testing.T) {
	store := newMockStore()
	srv := NewServer(Config{
		Store: store,
		Addr:  ":0",
	})
	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(func() { ts.Close() })

	// Connect via raw TCP for the WebSocket handshake.
	addr := strings.TrimPrefix(ts.URL, "http://")
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("dialing test server: %v", err)
	}
	defer conn.Close()

	// Perform WebSocket handshake.
	wsKey := base64.StdEncoding.EncodeToString([]byte("test-key-12345678"))
	handshake := fmt.Sprintf("GET /ws HTTP/1.1\r\n"+
		"Host: %s\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Key: %s\r\n"+
		"Sec-WebSocket-Version: 13\r\n"+
		"\r\n", addr, wsKey)

	if _, err := conn.Write([]byte(handshake)); err != nil {
		t.Fatalf("writing handshake: %v", err)
	}

	// Read the upgrade response.
	reader := bufio.NewReader(conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("reading status line: %v", err)
	}
	if !strings.Contains(statusLine, "101") {
		t.Fatalf("expected 101 response, got: %s", strings.TrimSpace(statusLine))
	}

	// Read remaining headers.
	var acceptKey string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("reading header: %v", err)
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Sec-WebSocket-Accept:") {
			acceptKey = strings.TrimSpace(strings.TrimPrefix(line, "Sec-WebSocket-Accept:"))
		}
	}

	// Verify accept key.
	expectedAccept := computeAcceptKey(wsKey)
	if acceptKey != expectedAccept {
		t.Errorf("expected accept key %q, got %q", expectedAccept, acceptKey)
	}

	// Verify client is registered.
	// Give the server a moment to register the client.
	time.Sleep(50 * time.Millisecond)
	if count := srv.hub.clientCount(); count != 1 {
		t.Errorf("expected 1 WebSocket client, got %d", count)
	}

	// Broadcast an event and verify it is received.
	srv.hub.Broadcast("agent_state_change", map[string]string{
		"agent_id":   "test-agent",
		"old_status": "RUNNING",
		"new_status": "STOPPED",
	})

	// Read the broadcast frame.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	opcode, payload, err := wsReadFrame(conn)
	if err != nil {
		t.Fatalf("reading WebSocket frame: %v", err)
	}
	if opcode != wsOpText {
		t.Fatalf("expected text opcode (0x1), got 0x%x", opcode)
	}

	var event wsEvent
	if err := json.Unmarshal(payload, &event); err != nil {
		t.Fatalf("unmarshaling WebSocket event: %v", err)
	}
	if event.Type != "agent_state_change" {
		t.Errorf("expected event type 'agent_state_change', got %q", event.Type)
	}

	data, ok := event.Data.(map[string]interface{})
	if !ok {
		t.Fatal("expected event data to be a JSON object")
	}
	if data["agent_id"] != "test-agent" {
		t.Errorf("expected agent_id 'test-agent', got %v", data["agent_id"])
	}

	// Send a close frame.
	closeMask := [4]byte{0x01, 0x02, 0x03, 0x04}
	closeFrame := []byte{
		0x88,       // FIN + close opcode
		0x80 | 0x0, // masked, 0 payload
	}
	closeFrame = append(closeFrame, closeMask[:]...)
	conn.Write(closeFrame)
}

func TestServer_WebSocket_BadUpgrade(t *testing.T) {
	ts, _ := setupTestServer(t)

	// Test without Upgrade header.
	resp, err := http.Get(ts.URL + "/ws")
	if err != nil {
		t.Fatalf("GET /ws failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", resp.StatusCode)
	}
}

func TestServer_CORSHeaders(t *testing.T) {
	ts, _ := setupTestServer(t)

	resp, err := http.Get(ts.URL + "/api/cluster")
	if err != nil {
		t.Fatalf("GET /api/cluster failed: %v", err)
	}
	defer resp.Body.Close()

	// Verify CORS headers (default is localhost).
	if origin := resp.Header.Get("Access-Control-Allow-Origin"); origin != "http://localhost:8080" {
		t.Errorf("expected Access-Control-Allow-Origin: http://localhost:8080, got %q", origin)
	}
	if methods := resp.Header.Get("Access-Control-Allow-Methods"); methods == "" {
		t.Error("expected Access-Control-Allow-Methods header to be present")
	}
	if headers := resp.Header.Get("Access-Control-Allow-Headers"); headers == "" {
		t.Error("expected Access-Control-Allow-Headers header to be present")
	}
}

func TestServer_CORSPreflight(t *testing.T) {
	ts, _ := setupTestServer(t)

	req, err := http.NewRequest(http.MethodOptions, ts.URL+"/api/agents", nil)
	if err != nil {
		t.Fatalf("creating OPTIONS request: %v", err)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS /api/agents failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("expected status 204, got %d", resp.StatusCode)
	}
	if origin := resp.Header.Get("Access-Control-Allow-Origin"); origin != "http://localhost:8080" {
		t.Errorf("expected Access-Control-Allow-Origin: http://localhost:8080, got %q", origin)
	}
}

func TestServer_MethodNotAllowed(t *testing.T) {
	ts, _ := setupTestServer(t)

	// POST to a GET-only endpoint.
	resp, err := http.Post(ts.URL+"/api/cluster", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /api/cluster failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected status 405, got %d", resp.StatusCode)
	}
}

func TestServer_ContentType(t *testing.T) {
	ts, _ := setupTestServer(t)

	resp, err := http.Get(ts.URL + "/api/cluster")
	if err != nil {
		t.Fatalf("GET /api/cluster failed: %v", err)
	}
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("expected Content-Type application/json, got %q", ct)
	}
}

func TestServer_StaticFiles(t *testing.T) {
	ts, _ := setupTestServer(t)

	resp, err := http.Get(ts.URL + "/index.html")
	if err != nil {
		t.Fatalf("GET /index.html failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200 for static file, got %d", resp.StatusCode)
	}
}

func TestServer_StopGraceful(t *testing.T) {
	store := newMockStore()
	srv := NewServer(Config{
		Store: store,
		Addr:  "127.0.0.1:0",
	})

	// Start in background.
	go srv.Start()
	time.Sleep(100 * time.Millisecond)

	// Stop gracefully.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("stopping server: %v", err)
	}
}

func TestExtractPathParam(t *testing.T) {
	tests := []struct {
		path   string
		prefix string
		want   string
	}{
		{"/api/agents/foo", "/api/agents/", "foo"},
		{"/api/agents/", "/api/agents/", ""},
		{"/api/agents/foo/chat", "/api/agents/", "foo"},
		{"/api/nodes/node-42", "/api/nodes/", "node-42"},
		{"/api/logs/agent-1", "/api/logs/", "agent-1"},
		{"/other/path", "/api/agents/", ""},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := extractPathParam(tt.path, tt.prefix)
			if got != tt.want {
				t.Errorf("extractPathParam(%q, %q) = %q, want %q", tt.path, tt.prefix, got, tt.want)
			}
		})
	}
}

func TestComputeAcceptKey(t *testing.T) {
	// RFC 6455 Section 4.2.2 example.
	key := "dGhlIHNhbXBsZSBub25jZQ=="
	expected := computeExpectedAcceptKey(key)
	got := computeAcceptKey(key)
	if got != expected {
		t.Errorf("computeAcceptKey(%q) = %q, want %q", key, got, expected)
	}
}

func computeExpectedAcceptKey(key string) string {
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte("258EAFA5-E914-47DA-95CA-5AB9DC11B65A"))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func TestWSHub_BroadcastNoClients(t *testing.T) {
	hub := newWSHub(slog.Default())
	// Should not panic with no clients.
	hub.Broadcast("test", map[string]string{"key": "value"})
}

func TestWSHub_RegisterUnregister(t *testing.T) {
	hub := newWSHub(slog.Default())

	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	client := &wsClient{
		conn: c1,
		send: make(chan []byte, 10),
	}

	hub.register(client)
	if hub.clientCount() != 1 {
		t.Errorf("expected 1 client, got %d", hub.clientCount())
	}

	hub.unregister(client)
	if hub.clientCount() != 0 {
		t.Errorf("expected 0 clients, got %d", hub.clientCount())
	}

	// Double unregister should not panic.
	hub.unregister(client)
}

func TestWSFrameRoundTrip(t *testing.T) {
	// Test WebSocket frame encoding/decoding round-trip.
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	message := []byte(`{"type":"test","data":"hello"}`)

	// Write from server side (unmasked).
	go func() {
		if err := wsWriteFrame(c1, wsOpText, message); err != nil {
			t.Errorf("writing frame: %v", err)
		}
	}()

	// Read on client side (will be unmasked since server sent it).
	opcode, payload, err := wsReadFrame(c2)
	if err != nil {
		t.Fatalf("reading frame: %v", err)
	}
	if opcode != wsOpText {
		t.Errorf("expected opcode 0x1, got 0x%x", opcode)
	}
	if string(payload) != string(message) {
		t.Errorf("expected payload %q, got %q", message, payload)
	}
}

func TestWSFrameLargePayload(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	// Test with payload > 125 bytes (uses 2-byte extended length).
	message := make([]byte, 1000)
	for i := range message {
		message[i] = byte('A' + (i % 26))
	}

	go func() {
		if err := wsWriteFrame(c1, wsOpText, message); err != nil {
			t.Errorf("writing large frame: %v", err)
		}
	}()

	opcode, payload, err := wsReadFrame(c2)
	if err != nil {
		t.Fatalf("reading large frame: %v", err)
	}
	if opcode != wsOpText {
		t.Errorf("expected opcode 0x1, got 0x%x", opcode)
	}
	if len(payload) != len(message) {
		t.Errorf("expected payload length %d, got %d", len(message), len(payload))
	}
}

func TestWSMaskedFrame(t *testing.T) {
	// Test reading a masked frame (as a client would send).
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	payload := []byte("hello")
	maskKey := [4]byte{0x37, 0xfa, 0x21, 0x3d}

	go func() {
		// Write a masked frame manually.
		frame := []byte{
			0x81,                     // FIN + text opcode
			0x80 | byte(len(payload)), // masked + length
		}
		frame = append(frame, maskKey[:]...)

		// Mask the payload.
		masked := make([]byte, len(payload))
		for i := range payload {
			masked[i] = payload[i] ^ maskKey[i%4]
		}
		frame = append(frame, masked...)

		c1.Write(frame)
	}()

	opcode, got, err := wsReadFrame(c2)
	if err != nil {
		t.Fatalf("reading masked frame: %v", err)
	}
	if opcode != wsOpText {
		t.Errorf("expected opcode 0x1, got 0x%x", opcode)
	}
	if string(got) != "hello" {
		t.Errorf("expected unmasked payload 'hello', got %q", got)
	}
}

func TestWSWriteFrameLengths(t *testing.T) {
	tests := []struct {
		name    string
		size    int
	}{
		{"tiny", 5},
		{"max_small", 125},
		{"medium", 126},
		{"large", 500},
		{"extended_16bit", 65535},
		{"extended_64bit", 65536},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c1, c2 := net.Pipe()
			defer c1.Close()
			defer c2.Close()

			payload := make([]byte, tt.size)
			for i := range payload {
				payload[i] = byte(i % 256)
			}

			go func() {
				wsWriteFrame(c1, wsOpText, payload)
			}()

			_, got, err := wsReadFrame(c2)
			if err != nil {
				t.Fatalf("reading frame of size %d: %v", tt.size, err)
			}
			if len(got) != tt.size {
				t.Errorf("expected %d bytes, got %d", tt.size, len(got))
			}
		})
	}
}

func TestHeaderContains(t *testing.T) {
	tests := []struct {
		header string
		token  string
		want   bool
	}{
		{"Upgrade", "Upgrade", true},
		{"upgrade", "Upgrade", true},
		{"keep-alive, Upgrade", "Upgrade", true},
		{"keep-alive, upgrade", "Upgrade", true},
		{"keep-alive", "Upgrade", false},
		{"", "Upgrade", false},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s_%s", tt.header, tt.token), func(t *testing.T) {
			got := headerContains(tt.header, tt.token)
			if got != tt.want {
				t.Errorf("headerContains(%q, %q) = %v, want %v", tt.header, tt.token, got, tt.want)
			}
		})
	}
}

// TestWSWriteFrame_CloseAndPing tests close and ping frame opcodes.
func TestWSWriteFrame_CloseAndPing(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	// Write a ping frame.
	go func() {
		wsWriteFrame(c1, wsOpPing, nil)
	}()

	opcode, payload, err := wsReadFrame(c2)
	if err != nil {
		t.Fatalf("reading ping frame: %v", err)
	}
	if opcode != wsOpPing {
		t.Errorf("expected ping opcode (0x9), got 0x%x", opcode)
	}
	if len(payload) != 0 {
		t.Errorf("expected empty payload, got %d bytes", len(payload))
	}
}

// TestWSWriteFrame_ClosePayload tests a close frame with a status code payload.
func TestWSWriteFrame_ClosePayload(t *testing.T) {
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()

	// Write a close frame with a 1000 (normal closure) status.
	closePayload := make([]byte, 2)
	binary.BigEndian.PutUint16(closePayload, 1000)

	go func() {
		wsWriteFrame(c1, wsOpClose, closePayload)
	}()

	opcode, payload, err := wsReadFrame(c2)
	if err != nil {
		t.Fatalf("reading close frame: %v", err)
	}
	if opcode != wsOpClose {
		t.Errorf("expected close opcode (0x8), got 0x%x", opcode)
	}
	if len(payload) != 2 {
		t.Fatalf("expected 2-byte close payload, got %d bytes", len(payload))
	}
	code := binary.BigEndian.Uint16(payload)
	if code != 1000 {
		t.Errorf("expected close code 1000, got %d", code)
	}
}
