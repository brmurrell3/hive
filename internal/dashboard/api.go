package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/auth"
	"github.com/brmurrell3/hive/internal/logs"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
	"nhooyr.io/websocket"
)

// StoreReader is the interface for reading state from the store.
type StoreReader interface {
	AllAgents() []*state.AgentState
	GetAgent(id string) *state.AgentState
	AllNodes() []*types.NodeState
	GetNode(id string) *types.NodeState
	GetCapabilityRegistry() *types.CapabilityRegistry
	AllUsers() []*auth.User
}

// NATSConn is the interface for NATS interaction.
type NATSConn interface {
	Subscribe(subject string, handler nats.MsgHandler) (*nats.Subscription, error)
	Publish(subject string, data []byte) error
}

// LogQuerier is the interface for querying aggregated logs.
type LogQuerier interface {
	Query(agentID string, opts logs.QueryOpts) ([]logs.LogEntry, error)
}

// Config holds the configuration for the dashboard API server.
type Config struct {
	Store      StoreReader
	NATSConn   NATSConn
	Logs       LogQuerier // optional; if nil, log queries return empty results
	Logger     *slog.Logger
	Addr       string // e.g. ":8080"
	CORSOrigin string // Allowed CORS origin; defaults to "*"
	AuthToken  string // If set, WebSocket connections must provide this token via ?token= query param
}

// Server is the dashboard API server.
type Server struct {
	store      StoreReader
	natsConn   NATSConn
	logs       LogQuerier
	logger     *slog.Logger
	httpServer *http.Server
	hub        *wsHub
	startTime  time.Time
	subs       []*nats.Subscription
	corsOrigin string // Allowed CORS origin
	authToken  string // Required token for WebSocket authentication
}

// NewServer creates a new dashboard API server.
func NewServer(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if cfg.CORSOrigin == "" {
		cfg.CORSOrigin = "*"
	}

	s := &Server{
		store:      cfg.Store,
		natsConn:   cfg.NATSConn,
		logs:       cfg.Logs,
		logger:     cfg.Logger,
		hub:        newWSHub(cfg.Logger),
		startTime:  time.Now(),
		corsOrigin: cfg.CORSOrigin,
		authToken:  cfg.AuthToken,
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	s.httpServer = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.corsMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	return s
}

// Start starts the HTTP server and subscribes to NATS subjects for live events.
func (s *Server) Start() error {
	if s.natsConn != nil {
		if err := s.subscribeNATS(); err != nil {
			return fmt.Errorf("subscribing to NATS: %w", err)
		}
	}

	s.logger.Info("dashboard API server starting", "addr", s.httpServer.Addr)
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("starting HTTP server: %w", err)
	}
	return nil
}

// Stop gracefully shuts down the server.
func (s *Server) Stop(ctx context.Context) error {
	s.logger.Info("dashboard API server stopping")

	// Unsubscribe from NATS.
	for _, sub := range s.subs {
		if err := sub.Unsubscribe(); err != nil {
			s.logger.Warn("failed to unsubscribe from NATS", "error", err)
		}
	}

	// Close all WebSocket clients.
	s.hub.closeAll()

	// Shut down HTTP server.
	return s.httpServer.Shutdown(ctx)
}

// registerRoutes sets up all API routes.
func (s *Server) registerRoutes(mux *http.ServeMux) {
	// REST API endpoints.
	mux.HandleFunc("/api/cluster", s.handleCluster)
	mux.HandleFunc("/api/nodes", s.handleNodes)
	mux.HandleFunc("/api/nodes/", s.handleNodeDetail)
	mux.HandleFunc("/api/agents", s.handleAgents)
	mux.HandleFunc("/api/agents/", s.handleAgentRoutes)
	mux.HandleFunc("/api/capabilities", s.handleCapabilities)
	mux.HandleFunc("/api/logs/", s.handleLogs)

	// WebSocket endpoint.
	mux.HandleFunc("/ws", s.handleWebSocket)

	// Static files (embedded).
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		s.logger.Error("failed to create static file sub-filesystem", "error", err)
	} else {
		mux.Handle("/", http.FileServer(http.FS(staticFS)))
	}
}

// corsMiddleware adds CORS headers to all responses.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", s.corsOrigin)
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// --- REST Handlers ---

// handleCluster returns a cluster overview.
func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	agents := s.store.AllAgents()
	nodes := s.store.AllNodes()

	// Count unique teams from agents.
	teams := make(map[string]bool)
	for _, a := range agents {
		if a.Team != "" {
			teams[a.Team] = true
		}
	}

	// Count agents by status.
	statusCounts := make(map[string]int)
	for _, a := range agents {
		statusCounts[string(a.Status)]++
	}

	overview := map[string]interface{}{
		"node_count":     len(nodes),
		"team_count":     len(teams),
		"agent_count":    len(agents),
		"uptime_seconds": int(time.Since(s.startTime).Seconds()),
		"agent_status":   statusCounts,
	}

	s.writeJSON(w, http.StatusOK, overview)
}

// handleNodes returns a list of all nodes.
func (s *Server) handleNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	nodes := s.store.AllNodes()
	s.writeJSON(w, http.StatusOK, nodes)
}

// handleNodeDetail returns details for a specific node.
func (s *Server) handleNodeDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	id := extractPathParam(r.URL.Path, "/api/nodes/")
	if id == "" {
		s.writeError(w, http.StatusBadRequest, "node ID required")
		return
	}

	node := s.store.GetNode(id)
	if node == nil {
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("node %q not found", id))
		return
	}

	s.writeJSON(w, http.StatusOK, node)
}

// handleAgents returns a list of all agents with status.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	agents := s.store.AllAgents()
	s.writeJSON(w, http.StatusOK, agents)
}

// handleAgentRoutes dispatches agent sub-routes.
func (s *Server) handleAgentRoutes(w http.ResponseWriter, r *http.Request) {
	// Check for /api/agents/:id/chat
	path := r.URL.Path
	if strings.HasSuffix(path, "/chat") {
		s.handleAgentChat(w, r)
		return
	}
	s.handleAgentDetail(w, r)
}

// handleAgentDetail returns details for a specific agent.
func (s *Server) handleAgentDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	id := extractPathParam(r.URL.Path, "/api/agents/")
	if id == "" {
		s.writeError(w, http.StatusBadRequest, "agent ID required")
		return
	}

	agent := s.store.GetAgent(id)
	if agent == nil {
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("agent %q not found", id))
		return
	}

	s.writeJSON(w, http.StatusOK, agent)
}

// handleAgentChat sends a message to an agent and waits for a response.
func (s *Server) handleAgentChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if s.natsConn == nil {
		s.writeError(w, http.StatusServiceUnavailable, "NATS connection not available")
		return
	}

	// Extract agent ID: /api/agents/:id/chat
	path := strings.TrimSuffix(r.URL.Path, "/chat")
	id := extractPathParam(path, "/api/agents/")
	if id == "" {
		s.writeError(w, http.StatusBadRequest, "agent ID required")
		return
	}

	// Verify agent exists.
	agent := s.store.GetAgent(id)
	if agent == nil {
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("agent %q not found", id))
		return
	}

	// Parse request body.
	var req struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Message == "" {
		s.writeError(w, http.StatusBadRequest, "message is required")
		return
	}

	// Create a reply inbox.
	replySubject := fmt.Sprintf("_INBOX.dashboard.%s.%d", id, time.Now().UnixNano())

	// Subscribe to the reply subject.
	replyCh := make(chan []byte, 1)
	sub, err := s.natsConn.Subscribe(replySubject, func(msg *nats.Msg) {
		select {
		case replyCh <- msg.Data:
		default:
		}
	})
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to subscribe for reply")
		return
	}
	defer sub.Unsubscribe()

	// Build and publish the request envelope.
	envelope := types.Envelope{
		ID:        types.NewUUID(),
		From:      "dashboard",
		To:        id,
		Type:      types.MessageTypeTask,
		Timestamp: time.Now().UTC(),
		Payload:   map[string]string{"message": req.Message},
		ReplyTo:   replySubject,
	}

	envData, err := json.Marshal(envelope)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to marshal envelope")
		return
	}

	capSubject := fmt.Sprintf("hive.agent.%s.inbox", id)
	if err := s.natsConn.Publish(capSubject, envData); err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to publish message")
		return
	}

	// Wait for reply with timeout.
	select {
	case reply := <-replyCh:
		var resp interface{}
		if err := json.Unmarshal(reply, &resp); err != nil {
			// Return raw string if not valid JSON.
			s.writeJSON(w, http.StatusOK, map[string]interface{}{
				"agent_id": id,
				"response": string(reply),
			})
			return
		}
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"agent_id": id,
			"response": resp,
		})
	case <-time.After(10 * time.Second):
		s.writeError(w, http.StatusGatewayTimeout, "agent did not respond within 10 seconds")
	}
}

// handleCapabilities returns all registered capabilities.
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	reg := s.store.GetCapabilityRegistry()
	if reg == nil {
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"agents":       map[string]interface{}{},
			"capabilities": map[string][]string{},
		})
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"agents":       reg.Agents,
		"capabilities": reg.AllCapabilities(),
	})
}

// handleLogs returns log entries for an agent.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	agentID := extractPathParam(r.URL.Path, "/api/logs/")
	if agentID == "" {
		s.writeError(w, http.StatusBadRequest, "agent ID required")
		return
	}

	// Verify agent exists.
	agent := s.store.GetAgent(agentID)
	if agent == nil {
		s.writeError(w, http.StatusNotFound, fmt.Sprintf("agent %q not found", agentID))
		return
	}

	// If no log aggregator is configured, return empty results.
	if s.logs == nil {
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"agent_id": agentID,
			"entries":  []interface{}{},
		})
		return
	}

	// Parse query parameters into QueryOpts.
	var opts logs.QueryOpts

	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		if t, err := time.Parse(time.RFC3339, sinceStr); err == nil {
			opts.Since = t
		} else {
			s.writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid 'since' parameter: %s", err))
			return
		}
	}

	if untilStr := r.URL.Query().Get("until"); untilStr != "" {
		if t, err := time.Parse(time.RFC3339, untilStr); err == nil {
			opts.Until = t
		} else {
			s.writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid 'until' parameter: %s", err))
			return
		}
	}

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			opts.Limit = n
		} else {
			s.writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid 'limit' parameter: must be a positive integer"))
			return
		}
	}

	entries, err := s.logs.Query(agentID, opts)
	if err != nil {
		s.logger.Error("failed to query logs", "agent_id", agentID, "error", err)
		s.writeError(w, http.StatusInternalServerError, "failed to query logs")
		return
	}

	// Ensure we return an empty array rather than null when there are no entries.
	if entries == nil {
		entries = []logs.LogEntry{}
	}

	s.writeJSON(w, http.StatusOK, map[string]interface{}{
		"agent_id": agentID,
		"entries":  entries,
	})
}

// --- JSON Response Helpers ---

func (s *Server) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(data); err != nil {
		s.logger.Error("failed to write JSON response", "error", err)
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, message string) {
	s.writeJSON(w, status, map[string]string{"error": message})
}

// extractPathParam extracts the remaining path after a prefix.
// For example, extractPathParam("/api/agents/foo", "/api/agents/") returns "foo".
func extractPathParam(path, prefix string) string {
	if !strings.HasPrefix(path, prefix) {
		return ""
	}
	param := strings.TrimPrefix(path, prefix)
	// Strip trailing slashes and any sub-paths.
	if idx := strings.Index(param, "/"); idx >= 0 {
		param = param[:idx]
	}
	return param
}

// --- NATS Subscriptions for Live Events ---

func (s *Server) subscribeNATS() error {
	subjects := []struct {
		subject   string
		eventType string
	}{
		{"hive.health.>", "heartbeat"},
		{"hive.logs.>", "log_entry"},
		{"hive.agent.state.>", "agent_state_change"},
	}

	for _, sub := range subjects {
		eventType := sub.eventType
		natsSub, err := s.natsConn.Subscribe(sub.subject, func(msg *nats.Msg) {
			s.hub.Broadcast(eventType, json.RawMessage(msg.Data))
		})
		if err != nil {
			return fmt.Errorf("subscribing to %s: %w", sub.subject, err)
		}
		s.subs = append(s.subs, natsSub)
		s.logger.Debug("subscribed to NATS subject", "subject", sub.subject)
	}

	return nil
}

// --- WebSocket Implementation (nhooyr.io/websocket) ---

// handleWebSocket handles the WebSocket upgrade and connection using nhooyr.io/websocket.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	// Verify auth token if configured.
	if s.authToken != "" {
		token := r.URL.Query().Get("token")
		if token == "" {
			s.writeError(w, http.StatusUnauthorized, "authentication required: provide ?token= query parameter")
			return
		}
		if token != s.authToken {
			s.writeError(w, http.StatusUnauthorized, "invalid authentication token")
			return
		}
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		s.logger.Error("failed to accept WebSocket connection", "error", err)
		return
	}

	// Create and register the client.
	client := &wsClient{
		conn: conn,
		send: make(chan []byte, 256),
	}
	s.hub.register(client)

	s.logger.Debug("WebSocket client connected")

	// Start writer goroutine.
	go s.wsWriter(client)

	// Reader loop (blocks until connection closes).
	s.wsReader(client)
}

// wsReader reads messages from the WebSocket connection.
// Dashboard WebSocket is server-push only; we just drain client messages.
func (s *Server) wsReader(client *wsClient) {
	defer func() {
		s.hub.unregister(client)
		client.conn.Close(websocket.StatusNormalClosure, "")
		s.logger.Debug("WebSocket client disconnected")
	}()

	for {
		_, _, err := client.conn.Read(context.Background())
		if err != nil {
			return
		}
		// Ignore all client messages (server-push only).
	}
}

// wsWriter writes queued messages to the WebSocket connection.
func (s *Server) wsWriter(client *wsClient) {
	for data := range client.send {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := client.conn.Write(ctx, websocket.MessageText, data)
		cancel()
		if err != nil {
			s.logger.Debug("failed to write WebSocket message", "error", err)
			return
		}
	}
}

// --- WebSocket Hub ---

// wsHub manages connected WebSocket clients.
type wsHub struct {
	mu      sync.RWMutex
	clients map[*wsClient]bool
	logger  *slog.Logger
}

// wsClient represents a connected WebSocket client.
type wsClient struct {
	conn      *websocket.Conn
	send      chan []byte
	closeOnce sync.Once
}

// closeSend safely closes the send channel exactly once, preventing double-close panics.
func (c *wsClient) closeSend() {
	c.closeOnce.Do(func() {
		close(c.send)
	})
}

// trySend attempts to send a message on the client's send channel.
// Returns false if the channel is full or closed.
func (c *wsClient) trySend(msg []byte) (sent bool) {
	defer func() {
		if r := recover(); r != nil {
			// send channel was closed between the map lookup and the send.
			sent = false
		}
	}()
	select {
	case c.send <- msg:
		return true
	default:
		return false
	}
}

// wsEvent is the JSON structure broadcast over WebSocket.
type wsEvent struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

func newWSHub(logger *slog.Logger) *wsHub {
	return &wsHub{
		clients: make(map[*wsClient]bool),
		logger:  logger,
	}
}

// register adds a client to the hub.
func (h *wsHub) register(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c] = true
	h.logger.Debug("WebSocket client registered", "total_clients", len(h.clients))
}

// unregister removes a client from the hub and closes its send channel.
func (h *wsHub) unregister(c *wsClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; ok {
		delete(h.clients, c)
		c.closeSend()
		h.logger.Debug("WebSocket client unregistered", "total_clients", len(h.clients))
	}
}

// Broadcast sends an event to all connected WebSocket clients.
func (h *wsHub) Broadcast(eventType string, data interface{}) {
	event := wsEvent{
		Type: eventType,
		Data: data,
	}

	msg, err := json.Marshal(event)
	if err != nil {
		h.logger.Error("failed to marshal WebSocket event", "error", err)
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	for client := range h.clients {
		if !client.trySend(msg) {
			// Client send buffer is full or channel was closed; skip to avoid blocking.
			h.logger.Warn("WebSocket client send buffer full, dropping message")
		}
	}
}

// closeAll closes all connected clients.
func (h *wsHub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()

	for client := range h.clients {
		client.closeSend()
		client.conn.Close(websocket.StatusGoingAway, "server shutting down")
		delete(h.clients, client)
	}
}

// clientCount returns the number of connected clients.
func (h *wsHub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}
