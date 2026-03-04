// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package dashboard provides the REST and WebSocket API for the Hive control plane UI, with per-user RBAC.
package dashboard

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brmurrell3/hive/internal/auth"
	"github.com/brmurrell3/hive/internal/logs"
	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/coder/websocket"
	"github.com/nats-io/nats.go"
	"golang.org/x/time/rate"
)

// contextKey is an unexported type for context keys in this package,
// preventing collisions with keys defined in other packages.
type contextKey int

const (
	// ctxKeyUser is the context key for the authenticated *auth.User.
	ctxKeyUser contextKey = iota
)

// Dashboard timeout and duration constants.
const (
	// httpReadHeaderTimeout is the timeout for reading HTTP request headers.
	httpReadHeaderTimeout = 10 * time.Second
	// httpReadTimeout is the timeout for reading the full HTTP request body.
	httpReadTimeout = 30 * time.Second
	// httpWriteTimeout is the timeout for writing the HTTP response.
	httpWriteTimeout = 60 * time.Second
	// httpIdleTimeout is the timeout for idle keep-alive connections.
	httpIdleTimeout = 120 * time.Second
	// chatReplyTimeout is how long the /chat endpoint waits for an agent response.
	chatReplyTimeout = 10 * time.Second
	// wsWriteTimeout is the timeout for writing a single WebSocket message or ping.
	wsWriteTimeout = 5 * time.Second
	// natsBroadcastMaxSize is the maximum size of a NATS broadcast message (1MB).
	natsBroadcastMaxSize = 1024 * 1024
	// wsReadLimit is the maximum incoming WebSocket message size (1MB).
	wsReadLimit = 1024 * 1024
	// dropWarnInterval is how often the hub logs about dropped messages per client.
	dropWarnInterval = time.Minute
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
	Request(subject string, data []byte, timeout time.Duration) (*nats.Msg, error)
}

// LogQuerier is the interface for querying aggregated logs.
type LogQuerier interface {
	Query(agentID string, opts logs.QueryOpts) ([]logs.LogEntry, error)
}

// Config holds the configuration for the dashboard API server.
type Config struct {
	Store          StoreReader
	NATSConn       NATSConn
	Logs           LogQuerier // optional; if nil, log queries return empty results
	Logger         *slog.Logger
	Addr           string           // e.g. ":8080"
	CORSOrigin     string           // Allowed CORS origin; defaults to same-origin only (empty string)
	AllowedOrigins []string         // Allowed WebSocket origins; defaults to dashboard's own address; ["*"] allows all
	AuthToken      string           // If set, API and WebSocket connections require this token
	Authorizer     *auth.Authorizer // If set, per-user RBAC is enforced; tokens are matched against user token hashes
}

// UserResponse is the JSON representation of a user in API responses.
// It omits the TokenHash field to avoid leaking sensitive data.
type UserResponse struct {
	ID     string    `json:"id"`
	Name   string    `json:"name,omitempty"`
	Role   auth.Role `json:"role"`
	Teams  []string  `json:"teams,omitempty"`
	Agents []string  `json:"agents,omitempty"`
}

// toUserResponse converts an auth.User to a UserResponse, stripping the token hash.
func toUserResponse(u *auth.User) UserResponse {
	return UserResponse{
		ID:     u.ID,
		Name:   u.Name,
		Role:   u.Role,
		Teams:  u.Teams,
		Agents: u.Agents,
	}
}

// toUserResponses converts a slice of auth.User pointers to UserResponse values.
func toUserResponses(users []*auth.User) []UserResponse {
	result := make([]UserResponse, len(users))
	for i, u := range users {
		result[i] = toUserResponse(u)
	}
	return result
}

// Server is the dashboard API server.
type Server struct {
	store          StoreReader
	natsConn       NATSConn
	logs           LogQuerier
	logger         *slog.Logger
	httpServer     *http.Server
	hub            *wsHub
	startTime      time.Time
	subs           []*nats.Subscription
	corsOrigin     string           // Allowed CORS origin
	allowedOrigins []string         // Allowed WebSocket origins
	authToken      string           // Required token for API and WebSocket authentication
	authorizer     *auth.Authorizer // Per-user RBAC authorizer (nil = shared token only)
	healthzLimiter *rate.Limiter
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
}

// NewServer creates a new dashboard API server.
func NewServer(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}

	// Derive localhost origins from the listen address for same-origin default.
	host := cfg.Addr
	if strings.HasPrefix(host, ":") {
		host = "localhost" + host
	}
	localhostOrigins := []string{"http://" + host, "https://" + host}

	// Default CORS to same-origin only (empty string means no Access-Control-Allow-Origin header).
	// Wildcard "*" is not allowed in production — treat it as localhost-only and log a warning.
	corsOrigin := cfg.CORSOrigin
	if corsOrigin == "*" {
		cfg.Logger.Warn("CORSOrigin=\"*\" is not safe for production; restricting to localhost origins",
			"allowed_origins", localhostOrigins)
		corsOrigin = ""
	}

	// Default WebSocket allowed origins to the dashboard's own address.
	// Wildcard "*" is not allowed — treat it as localhost-only and log a warning.
	allowedOrigins := cfg.AllowedOrigins
	if len(allowedOrigins) == 1 && allowedOrigins[0] == "*" {
		cfg.Logger.Warn("WebSocket AllowedOrigins=[\"*\"] is not safe for production; restricting to localhost origins",
			"allowed_origins", localhostOrigins)
		allowedOrigins = localhostOrigins
	}
	if len(allowedOrigins) == 0 {
		allowedOrigins = localhostOrigins
	}

	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())

	s := &Server{
		store:          cfg.Store,
		natsConn:       cfg.NATSConn,
		logs:           cfg.Logs,
		logger:         cfg.Logger,
		hub:            newWSHub(cfg.Logger, cfg.Store),
		startTime:      time.Now(),
		corsOrigin:     corsOrigin,
		allowedOrigins: allowedOrigins,
		authToken:      cfg.AuthToken,
		authorizer:     cfg.Authorizer,
		healthzLimiter: rate.NewLimiter(100, 100),
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
	}

	mux := http.NewServeMux()
	s.registerRoutes(mux)

	s.httpServer = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.corsMiddleware(mux),
		ReadHeaderTimeout: httpReadHeaderTimeout,
		ReadTimeout:       httpReadTimeout,
		WriteTimeout:      httpWriteTimeout,
		IdleTimeout:       httpIdleTimeout,
		MaxHeaderBytes:    1 << 16, // 64KB
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
	if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("starting HTTP server: %w", err)
	}
	return nil
}

// Stop gracefully shuts down the server.
func (s *Server) Stop(ctx context.Context) error {
	s.logger.Info("dashboard API server stopping")

	s.shutdownCancel()

	for _, sub := range s.subs {
		if err := sub.Unsubscribe(); err != nil {
			s.logger.Warn("failed to unsubscribe from NATS", "error", err)
		}
	}

	s.hub.closeAll()

	return s.httpServer.Shutdown(ctx)
}

// registerRoutes sets up all API routes.
func (s *Server) registerRoutes(mux *http.ServeMux) {
	// REST API endpoints (all protected by auth middleware with RBAC action).
	// Read endpoints require "list" action; write endpoints require specific actions.
	mux.HandleFunc("/api/cluster", s.authMiddleware("status", s.handleCluster))
	mux.HandleFunc("/api/nodes", s.authMiddleware("list", s.handleNodes))
	mux.HandleFunc("/api/nodes/", s.authMiddleware("status", s.handleNodeDetail))
	mux.HandleFunc("/api/agents", s.authMiddleware("list", s.handleAgents))
	mux.HandleFunc("/api/agents/", s.authMiddleware("", s.handleAgentRoutes)) // action determined per sub-route
	mux.HandleFunc("/api/capabilities", s.authMiddleware("list", s.handleCapabilities))
	mux.HandleFunc("/api/logs/", s.authMiddleware("logs", s.handleLogs))
	mux.HandleFunc("/api/users", s.authMiddleware("list", s.handleUsers))

	// Health check endpoint (no auth required).
	mux.HandleFunc("/healthz", s.handleHealthz)

	// WebSocket endpoint (uses its own token-based auth).
	mux.HandleFunc("/ws", s.handleWebSocket)

	// Static files are served without auth to allow the SPA to load before client-side authentication.
	// M-1: Wrap with cache-control middleware for appropriate caching headers.
	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		s.logger.Error("failed to create static file sub-filesystem", "error", err)
	} else {
		mux.Handle("/", cacheControlMiddleware(http.FileServer(http.FS(staticFS))))
	}
}

// authMiddleware checks the Authorization: Bearer <token> header and enforces RBAC.
//
// Authentication is two-tier for backward compatibility:
//  1. If the token matches the dashboard shared token (authToken), the request is
//     treated as admin (existing behavior, no per-user RBAC).
//  2. If an Authorizer is configured and the token matches a user's token hash,
//     the request is authenticated as that user and RBAC is enforced.
//
// The requiredAction parameter specifies the RBAC action to check (e.g. "list",
// "status", "chat"). An empty string skips the RBAC check (useful when
// the handler determines the action itself based on the sub-route).
//
// If no auth token is configured (dev mode), all requests are allowed.
func (s *Server) authMiddleware(requiredAction string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// If no auth token is configured, skip auth (dev mode).
		if s.authToken == "" {
			next(w, r)
			return
		}

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		const prefix = "Bearer "
		if !strings.HasPrefix(authHeader, prefix) {
			http.Error(w, "invalid authorization format", http.StatusUnauthorized)
			return
		}

		token := authHeader[len(prefix):]

		// Path 1: Check against the shared dashboard token (admin access).
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.authToken)) == 1 {
			next(w, r)
			return
		}

		// Path 2: Check against per-user tokens via the Authorizer.
		if s.authorizer != nil {
			user, err := s.authorizer.Authenticate(token)
			if err == nil {
				// User authenticated. Check RBAC if an action is specified.
				if requiredAction != "" {
					if err := s.authorizer.Authorize(user, requiredAction, ""); err != nil {
						http.Error(w, "forbidden", http.StatusForbidden)
						return
					}
				}
				// Store user in context for downstream handlers that need it
				// (e.g., handleUsers checks for admin role, handleAgentRoutes
				// determines action based on the sub-route).
				ctx := context.WithValue(r.Context(), ctxKeyUser, user)
				next(w, r.WithContext(ctx))
				return
			}
		}

		// Neither path matched.
		http.Error(w, "invalid token", http.StatusUnauthorized)
	}
}

// userFromContext retrieves the authenticated user from the request context.
// Returns nil if the request was authenticated via the shared dashboard token
// (admin access) or if no user-level auth was performed.
func userFromContext(r *http.Request) *auth.User {
	u, _ := r.Context().Value(ctxKeyUser).(*auth.User)
	return u
}

// filterAgentsByUser filters agents based on the authenticated user's scope.
// Admin users and shared-token (nil user) requests see all agents.
func (s *Server) filterAgentsByUser(r *http.Request, agents []*state.AgentState) []*state.AgentState {
	user := userFromContext(r)
	if user == nil || user.Role == auth.RoleAdmin {
		return agents
	}
	if len(user.Teams) == 0 && len(user.Agents) == 0 {
		return nil // deny-by-default: empty scope sees nothing
	}
	teams := make(map[string]bool, len(user.Teams))
	for _, t := range user.Teams {
		teams[t] = true
	}
	allowed := make(map[string]bool, len(user.Agents))
	for _, a := range user.Agents {
		allowed[a] = true
	}
	var filtered []*state.AgentState
	for _, a := range agents {
		if teams[a.Team] || allowed[a.ID] {
			filtered = append(filtered, a)
		}
	}
	return filtered
}

// filterNodesByUser filters nodes based on the authenticated user's scope.
// Admin users and shared-token (nil user) requests see all nodes.
// Nodes are filtered by checking if any of the node's agents are accessible
// to the user.
func (s *Server) filterNodesByUser(r *http.Request, nodes []*types.NodeState) []*types.NodeState {
	user := userFromContext(r)
	if user == nil || user.Role == auth.RoleAdmin {
		return nodes
	}
	if len(user.Teams) == 0 && len(user.Agents) == 0 {
		return nil // deny-by-default: empty scope sees nothing
	}
	allowedAgents := make(map[string]bool, len(user.Agents))
	for _, a := range user.Agents {
		allowedAgents[a] = true
	}
	teams := make(map[string]bool, len(user.Teams))
	for _, t := range user.Teams {
		teams[t] = true
	}
	allAgents := s.store.AllAgents()
	agentTeam := make(map[string]string, len(allAgents))
	for _, a := range allAgents {
		agentTeam[a.ID] = a.Team
	}
	var filtered []*types.NodeState
	for _, n := range nodes {
		for _, agentID := range n.Agents {
			if allowedAgents[agentID] || teams[agentTeam[agentID]] {
				filtered = append(filtered, n)
				break
			}
		}
	}
	return filtered
}

// requireMethod checks that the request uses the expected HTTP method.
// Returns true if the method matches; writes a 405 error and returns false otherwise.
func (s *Server) requireMethod(w http.ResponseWriter, r *http.Request, method string) bool {
	if r.Method != method {
		s.writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return false
	}
	return true
}

// resolveAgent extracts, validates, looks up, and RBAC-filters an agent by ID
// from the URL path. Returns nil and writes an appropriate error if any step fails.
func (s *Server) resolveAgent(w http.ResponseWriter, r *http.Request, prefix string) *state.AgentState {
	id := extractPathParam(r.URL.Path, prefix)
	if id == "" {
		s.writeError(w, http.StatusBadRequest, "agent ID required")
		return nil
	}
	if err := types.ValidateSubjectComponent("agent_id", id); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid agent ID")
		return nil
	}
	agent := s.store.GetAgent(id)
	if agent == nil {
		s.writeError(w, http.StatusNotFound, "agent not found")
		return nil
	}
	filtered := s.filterAgentsByUser(r, []*state.AgentState{agent})
	if len(filtered) == 0 {
		s.writeError(w, http.StatusNotFound, "agent not found")
		return nil
	}
	return filtered[0]
}

// resolveNode extracts, validates, looks up, and RBAC-filters a node by ID
// from the URL path. Returns nil and writes an appropriate error if any step fails.
func (s *Server) resolveNode(w http.ResponseWriter, r *http.Request, prefix string) *types.NodeState {
	id := extractPathParam(r.URL.Path, prefix)
	if id == "" {
		s.writeError(w, http.StatusBadRequest, "node ID required")
		return nil
	}
	if err := types.ValidateSubjectComponent("node_id", id); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid node ID")
		return nil
	}
	node := s.store.GetNode(id)
	if node == nil {
		s.writeError(w, http.StatusNotFound, "node not found")
		return nil
	}
	filtered := s.filterNodesByUser(r, []*types.NodeState{node})
	if len(filtered) == 0 {
		s.writeError(w, http.StatusNotFound, "node not found")
		return nil
	}
	return filtered[0]
}

// handleHealthz returns a simple health check (no auth required).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}
	if !s.healthzLimiter.Allow() {
		s.writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleUsers returns all users with token hashes stripped.
// When per-user RBAC is active, only admin users may access this endpoint.
func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}

	// If authenticated via per-user token, require admin role.
	if user := userFromContext(r); user != nil {
		if user.Role != auth.RoleAdmin {
			s.writeError(w, http.StatusForbidden, "admin role required")
			return
		}
	}

	users := s.store.AllUsers()
	s.writeJSON(w, http.StatusOK, toUserResponses(users))
}

// corsMiddleware adds CORS headers to all responses.
// If corsOrigin is empty, no CORS headers are set (same-origin only).
// Otherwise, the configured origin is used.
// Note: wildcard "*" is rejected in NewServer and downgraded to empty.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Security headers to prevent common attacks (always set, regardless of CORS).
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; frame-ancestors 'none'; base-uri 'self'")
		w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")

		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		}

		// M-12: Only set CORS headers when an Origin header is present.
		origin := r.Header.Get("Origin")
		if origin == "" {
			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		if s.corsOrigin != "" && origin == s.corsOrigin {
			w.Header().Set("Access-Control-Allow-Origin", s.corsOrigin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
			w.Header().Set("Vary", "Origin")
		}

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// cacheControlMiddleware sets appropriate Cache-Control headers for static files.
// HTML files and the root path get no-cache to ensure fresh content, while other
// static assets (JS, CSS, images) are cached for 1 hour.
func cacheControlMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".html") || r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=3600")
		}
		next.ServeHTTP(w, r)
	})
}

// --- REST Handlers ---

// handleCluster returns a cluster overview.
func (s *Server) handleCluster(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}

	agents := s.store.AllAgents()
	nodes := s.store.AllNodes()

	// Filter by user scope so non-admin users only see their own resources.
	agents = s.filterAgentsByUser(r, agents)
	nodes = s.filterNodesByUser(r, nodes)

	teams := make(map[string]bool)
	for _, a := range agents {
		if a.Team != "" {
			teams[a.Team] = true
		}
	}

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
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}

	nodes := s.store.AllNodes()
	nodes = s.filterNodesByUser(r, nodes)
	s.writeJSON(w, http.StatusOK, nodes)
}

// handleNodeDetail returns details for a specific node.
func (s *Server) handleNodeDetail(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}

	node := s.resolveNode(w, r, "/api/nodes/")
	if node == nil {
		return
	}

	s.writeJSON(w, http.StatusOK, node)
}

// handleAgents returns a list of all agents with status.
func (s *Server) handleAgents(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}

	agents := s.store.AllAgents()
	agents = s.filterAgentsByUser(r, agents)
	s.writeJSON(w, http.StatusOK, agents)
}

// handleAgentRoutes dispatches agent sub-routes with per-action RBAC.
func (s *Server) handleAgentRoutes(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// M-14: Extract agent ID for RBAC resource scoping.
	trimmedPath := strings.TrimSuffix(path, "/chat")
	id := extractPathParam(trimmedPath, "/api/agents/")

	if strings.HasSuffix(path, "/chat") {
		// Chat requires "chat" action.
		if user := userFromContext(r); user != nil && s.authorizer != nil {
			if err := s.authorizer.Authorize(user, "chat", id); err != nil {
				s.writeError(w, http.StatusForbidden, "forbidden")
				return
			}
		}
		s.handleAgentChat(w, r)
		return
	}
	// Agent detail is a read operation requiring "status" action.
	if user := userFromContext(r); user != nil && s.authorizer != nil {
		if err := s.authorizer.Authorize(user, "status", id); err != nil {
			s.writeError(w, http.StatusForbidden, "forbidden")
			return
		}
	}
	s.handleAgentDetail(w, r)
}

// handleAgentDetail returns details for a specific agent.
func (s *Server) handleAgentDetail(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}

	agent := s.resolveAgent(w, r, "/api/agents/")
	if agent == nil {
		return
	}

	s.writeJSON(w, http.StatusOK, agent)
}

// handleAgentChat sends a message to an agent and waits for a response.
func (s *Server) handleAgentChat(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodPost) {
		return
	}

	// H-1: Require application/json Content-Type to prevent CSRF via form submission.
	if ct := r.Header.Get("Content-Type"); ct != "application/json" {
		s.writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
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

	if err := types.ValidateSubjectComponent("agent_id", id); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid agent ID")
		return
	}

	agent := s.store.GetAgent(id)
	if agent == nil {
		s.writeError(w, http.StatusNotFound, "agent not found")
		return
	}

	if user := userFromContext(r); user != nil && s.authorizer != nil {
		if err := s.authorizer.Authorize(user, "chat", agent.Team); err != nil {
			if err2 := s.authorizer.Authorize(user, "chat", agent.ID); err2 != nil {
				s.writeError(w, http.StatusForbidden, "forbidden")
				return
			}
		}
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

	// Validate message length to prevent oversized payloads from reaching NATS.
	const maxMessageLen = 256 * 1024 // 256KB
	if len(req.Message) > maxMessageLen {
		s.writeError(w, http.StatusBadRequest, "message exceeds maximum length of 256KB")
		return
	}

	// Create a cryptographically random reply inbox to prevent prediction attacks.
	replySubject := nats.NewInbox()

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
	payloadBytes, err := json.Marshal(map[string]string{"message": req.Message})
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to marshal payload")
		return
	}

	envelope := types.Envelope{
		ID:        types.NewUUID(),
		From:      "dashboard",
		To:        id,
		Type:      types.MessageTypeTask,
		Timestamp: time.Now().UTC(),
		Payload:   payloadBytes,
		ReplyTo:   replySubject,
	}

	envData, err := json.Marshal(envelope)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to marshal envelope")
		return
	}

	capSubject := fmt.Sprintf(protocol.FmtAgentInbox, id)
	if err := s.natsConn.Publish(capSubject, envData); err != nil {
		s.writeError(w, http.StatusInternalServerError, "failed to publish message")
		return
	}

	// H-6: Use explicit timer to avoid time.After leak on early return paths.
	timer := time.NewTimer(chatReplyTimeout)
	defer timer.Stop()

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
	case <-r.Context().Done():
		sub.Unsubscribe()
		s.writeError(w, http.StatusGone, "client disconnected")
		return
	case <-timer.C:
		s.writeError(w, http.StatusGatewayTimeout, "agent did not respond in time")
	}
}

// handleCapabilities returns all registered capabilities.
func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
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

	// Filter capabilities by user scope. Admin users and shared-token (nil user)
	// see all capabilities; scoped users only see capabilities for their agents.
	user := userFromContext(r)
	if user != nil && user.Role != auth.RoleAdmin {
		// Build a set of agent IDs the user has access to.
		allAgents := s.store.AllAgents()
		allowedAgents := s.filterAgentsByUser(r, allAgents)
		allowedSet := make(map[string]bool, len(allowedAgents))
		for _, a := range allowedAgents {
			allowedSet[a.ID] = true
		}

		// Filter registry agents to only those in the user's scope.
		filteredAgents := make(map[string]*types.CapabilityRegistryEntry, len(reg.Agents))
		for agentID, entry := range reg.Agents {
			if allowedSet[agentID] {
				filteredAgents[agentID] = entry
			}
		}

		// Build filtered capabilities map (only include agents the user can see).
		filteredCaps := make(map[string][]string)
		for agentID, entry := range filteredAgents {
			for _, cap := range entry.Capabilities {
				filteredCaps[cap.Name] = append(filteredCaps[cap.Name], agentID)
			}
		}

		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"agents":       filteredAgents,
			"capabilities": filteredCaps,
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
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}

	agent := s.resolveAgent(w, r, "/api/logs/")
	if agent == nil {
		return
	}
	agentID := agent.ID

	// If no log aggregator is configured, return empty results.
	if s.logs == nil {
		s.writeJSON(w, http.StatusOK, map[string]interface{}{
			"agent_id": agentID,
			"entries":  []interface{}{},
		})
		return
	}

	var opts logs.QueryOpts

	if sinceStr := r.URL.Query().Get("since"); sinceStr != "" {
		if t, err := time.Parse(time.RFC3339, sinceStr); err == nil {
			opts.Since = t
		} else {
			s.writeError(w, http.StatusBadRequest, "invalid 'since' parameter: expected RFC3339 format")
			return
		}
	}

	if untilStr := r.URL.Query().Get("until"); untilStr != "" {
		if t, err := time.Parse(time.RFC3339, untilStr); err == nil {
			opts.Until = t
		} else {
			s.writeError(w, http.StatusBadRequest, "invalid 'until' parameter: expected RFC3339 format")
			return
		}
	}

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			if n > 10000 {
				n = 10000
			}
			opts.Limit = n
		} else {
			s.writeError(w, http.StatusBadRequest, "invalid 'limit' parameter: must be a positive integer")
			return
		}
	}
	// Cap the limit to prevent excessively large log queries.
	// When no limit is specified, default to 10000.
	if opts.Limit <= 0 || opts.Limit > 10000 {
		opts.Limit = 10000
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
	buf, err := json.Marshal(data)
	if err != nil {
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf) //nolint:errcheck // HTTP response write; error is unactionable
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
		{protocol.SubjLogsAll, "log_entry"},
		{"hive.agent.state.>", "agent_state_change"},
	}

	for _, sub := range subjects {
		eventType := sub.eventType
		natsSub, err := s.natsConn.Subscribe(sub.subject, func(msg *nats.Msg) {
			// C-6: Validate size and structure before broadcasting to WebSocket clients.
			if len(msg.Data) > natsBroadcastMaxSize {
				s.logger.Warn("NATS broadcast message too large, dropping", "size", len(msg.Data))
				return
			}
			if !json.Valid(msg.Data) {
				s.logger.Warn("NATS broadcast message is not valid JSON, dropping")
				return
			}
			// Extract the agent ID from the NATS subject for RBAC filtering.
			// Subjects follow patterns like hive.health.<agentID>,
			// hive.agent.state.<agentID>, hive.logs.<agentID>.
			agentID := extractAgentIDFromSubject(msg.Subject)
			s.hub.Broadcast(eventType, json.RawMessage(msg.Data), agentID)
		})
		if err != nil {
			return fmt.Errorf("subscribing to %s: %w", sub.subject, err)
		}
		if natsSub != nil {
			s.subs = append(s.subs, natsSub)
		}
		s.logger.Debug("subscribed to NATS subject", "subject", sub.subject)
	}

	return nil
}

// extractAgentIDFromSubject extracts the agent ID from NATS subjects used
// in live event subscriptions. The expected patterns are:
//
//	hive.health.<agentID>
//	hive.logs.<agentID>
//	hive.agent.state.<agentID>
//
// Returns an empty string if the subject does not match any known pattern.
func extractAgentIDFromSubject(subject string) string {
	parts := strings.Split(subject, ".")
	if len(parts) < 3 {
		return ""
	}
	// hive.health.<agentID> or hive.logs.<agentID>
	if len(parts) == 3 && (parts[1] == "health" || parts[1] == "logs") {
		return parts[2]
	}
	// hive.agent.state.<agentID>
	if len(parts) == 4 && parts[1] == "agent" && parts[2] == "state" {
		return parts[3]
	}
	return ""
}

// --- WebSocket Implementation (nhooyr.io/websocket) ---

// handleWebSocket handles the WebSocket upgrade and connection using nhooyr.io/websocket.
func (s *Server) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if !s.requireMethod(w, r, http.MethodGet) {
		return
	}

	// Verify auth token if configured.
	// Tokens are accepted from (in priority order):
	//   1. Authorization: Bearer <token> header (preferred)
	//   2. Sec-WebSocket-Protocol header carrying the token (standard WebSocket auth pattern)
	//   3. ?token= query parameter (deprecated; visible in logs/proxies — use headers instead)
	var wsProtocolAuth bool
	var wsUser *auth.User // H-2: track authenticated user for per-user RBAC
	if s.authToken != "" {
		token := ""
		if authHdr := r.Header.Get("Authorization"); strings.HasPrefix(authHdr, "Bearer ") {
			token = authHdr[len("Bearer "):]
		} else if proto := r.Header.Get("Sec-WebSocket-Protocol"); proto != "" {
			// The Sec-WebSocket-Protocol header can carry the token as a sub-protocol.
			// We check each comma-separated value against both the shared token
			// and per-user authentication to avoid silently ignoring per-user tokens.
			for _, p := range strings.Split(proto, ",") {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				// Try shared token first.
				if subtle.ConstantTimeCompare([]byte(p), []byte(s.authToken)) == 1 {
					token = p
					wsProtocolAuth = true
					break
				}
				// Try per-user authentication.
				if s.authorizer != nil {
					if u, err := s.authorizer.Authenticate(p); err == nil && u != nil {
						token = p
						wsProtocolAuth = true
						wsUser = u
						break
					}
				}
			}
		}
		if token == "" {
			// Fall back to query parameter (deprecated).
			token = r.URL.Query().Get("token")
			if token != "" {
				s.logger.Warn("DEPRECATED: WebSocket auth via query parameter exposes token in server logs and proxy caches; migrate to Authorization header or Sec-WebSocket-Protocol", "remote_addr", r.RemoteAddr)
			}
		}
		if token == "" {
			s.writeError(w, http.StatusUnauthorized, "authentication required: provide Authorization header, Sec-WebSocket-Protocol header, or ?token= query parameter")
			return
		}

		// H-2: Check shared token first, then try per-user RBAC authentication.
		//nolint:gocritic // ifElseChain — conditions are on different variables
		if subtle.ConstantTimeCompare([]byte(token), []byte(s.authToken)) == 1 {
			// Shared token = admin access, wsUser stays nil.
		} else if s.authorizer != nil {
			user, err := s.authorizer.Authenticate(token)
			if err != nil {
				s.writeError(w, http.StatusUnauthorized, "invalid authentication token")
				return
			}
			wsUser = user
		} else {
			s.writeError(w, http.StatusUnauthorized, "invalid authentication token")
			return
		}
	}

	acceptOpts := &websocket.AcceptOptions{
		OriginPatterns: s.allowedOrigins,
	}
	// If the client authenticated via Sec-WebSocket-Protocol, we must echo
	// a protocol back in the response (per RFC 6455 section 4.2.2).
	// Use a fixed protocol name instead of echoing the actual auth token,
	// which would leak the secret to any JavaScript running on the page.
	if wsProtocolAuth {
		acceptOpts.Subprotocols = []string{"hive-auth-v1"}
	}

	conn, err := websocket.Accept(w, r, acceptOpts)
	if err != nil {
		s.logger.Error("failed to accept WebSocket connection", "error", err)
		return
	}

	// Limit incoming WebSocket message size to prevent abuse.
	conn.SetReadLimit(wsReadLimit)

	// Create and register the client.
	client := &wsClient{
		conn: conn,
		send: make(chan []byte, 256),
		user: wsUser, // H-2: store authenticated user for potential future filtering
	}
	if !s.hub.register(client) {
		conn.Close(websocket.StatusTryAgainLater, "too many connections") //nolint:errcheck // best-effort websocket cleanup
		return
	}

	// L-8: Log remote address on WebSocket connect.
	s.logger.Debug("WebSocket client connected", "remote_addr", r.RemoteAddr)

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
		client.conn.Close(websocket.StatusNormalClosure, "") //nolint:errcheck // best-effort websocket cleanup
		s.logger.Debug("WebSocket client disconnected")
	}()

	for {
		_, _, err := client.conn.Read(s.shutdownCtx)
		if err != nil {
			return
		}
		// Ignore all client messages (server-push only).
	}
}

// wsPingInterval is how often we send WebSocket ping frames to keep the
// connection alive and detect dead peers. 30 seconds is a common choice
// that works well with most reverse proxies and load balancers.
const wsPingInterval = 30 * time.Second

// wsWriter writes queued messages to the WebSocket connection and sends
// periodic ping frames to keep the connection alive through proxies.
// On exit (write error, ping failure, or shutdown), the WebSocket connection
// is closed to unblock the wsReader goroutine.
func (s *Server) wsWriter(client *wsClient) {
	pingTicker := time.NewTicker(wsPingInterval)
	defer func() {
		pingTicker.Stop()
		// Close the connection so wsReader's blocking Read call returns an error
		// and the reader goroutine exits cleanly.
		client.conn.Close(websocket.StatusGoingAway, "writer exiting") //nolint:errcheck // best-effort websocket cleanup
	}()

	for {
		select {
		case data, ok := <-client.send:
			if !ok {
				// Channel closed; stop writing.
				return
			}
			ctx, cancel := context.WithTimeout(s.shutdownCtx, wsWriteTimeout)
			err := client.conn.Write(ctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				s.logger.Debug("failed to write WebSocket message", "error", err)
				return
			}

		case <-pingTicker.C:
			// Send a ping frame to keep the connection alive.
			// nhooyr.io/websocket handles pong responses automatically.
			ctx, cancel := context.WithTimeout(s.shutdownCtx, wsWriteTimeout)
			err := client.conn.Ping(ctx)
			cancel()
			if err != nil {
				s.logger.Debug("WebSocket ping failed", "error", err)
				return
			}

		case <-s.shutdownCtx.Done():
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
	store   StoreReader
}

// wsClient represents a connected WebSocket client.
type wsClient struct {
	conn         *websocket.Conn
	send         chan []byte
	closeOnce    sync.Once
	lastDropWarn atomic.Int64 // M-3: unix nanos of last "buffer full" warning
	dropCount    atomic.Int64 // M-3: count of dropped messages since last warning
	user         *auth.User   // H-2: authenticated user for per-user RBAC (nil = shared token / admin)
}

// closeSend safely closes the send channel exactly once, preventing double-close panics.
func (c *wsClient) closeSend() {
	c.closeOnce.Do(func() {
		close(c.send)
	})
}

// trySend attempts to send a message on the client's send channel.
// Returns false if the channel is full. The caller must hold at least
// an RLock on the hub mutex, which prevents unregister from closing
// the channel concurrently (unregister takes a write lock).
func (c *wsClient) trySend(msg []byte) bool {
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

func newWSHub(logger *slog.Logger, store StoreReader) *wsHub {
	return &wsHub{
		clients: make(map[*wsClient]bool),
		logger:  logger,
		store:   store,
	}
}

// maxWebSocketClients is the maximum number of concurrent WebSocket connections
// the hub will accept. New connections beyond this limit are rejected.
const maxWebSocketClients = 1000

// register adds a client to the hub. Returns false if the connection limit
// has been reached, in which case the caller should close the connection.
func (h *wsHub) register(c *wsClient) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) >= maxWebSocketClients {
		h.logger.Warn("WebSocket connection limit reached, rejecting new client",
			"max_clients", maxWebSocketClients)
		return false
	}
	h.clients[c] = true
	h.logger.Debug("WebSocket client registered", "total_clients", len(h.clients))
	return true
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
// Slow clients whose buffers are full are collected and removed after
// the read-locked iteration completes, avoiding concurrent map/channel
// mutation during broadcast.
// The agentID parameter enables per-user RBAC filtering: if non-empty,
// clients whose user scope does not include this agent are skipped.
func (h *wsHub) Broadcast(eventType string, data interface{}, agentID string) {
	event := wsEvent{
		Type: eventType,
		Data: data,
	}

	msg, err := json.Marshal(event)
	if err != nil {
		h.logger.Error("failed to marshal WebSocket event", "error", err)
		return
	}

	var slow []*wsClient

	h.mu.RLock()
	for client := range h.clients {
		// RBAC filtering: skip clients whose user scope does not include this agent.
		if agentID != "" && client.user != nil && client.user.Role != auth.RoleAdmin {
			if !userHasAgentAccess(client.user, agentID, h.store) {
				continue
			}
		}
		if !client.trySend(msg) {
			drops := client.dropCount.Add(1)
			lastWarn := time.Unix(0, client.lastDropWarn.Load())
			if time.Since(lastWarn) > dropWarnInterval {
				h.logger.Warn("WebSocket client send buffer full, dropping message",
					"dropped_since_last_warn", drops)
				client.lastDropWarn.Store(time.Now().UnixNano())
				client.dropCount.Store(0)
			}
			if drops > 256 {
				slow = append(slow, client)
			}
		}
	}
	h.mu.RUnlock()

	for _, c := range slow {
		h.mu.Lock()
		if _, ok := h.clients[c]; ok {
			delete(h.clients, c)
			c.closeSend()
			h.logger.Warn("evicted persistently slow WebSocket client")
		}
		h.mu.Unlock()
	}
}

// userHasAgentAccess checks whether a non-admin user's scope includes the
// given agent ID (by direct agent list or team membership).
// The store is used to resolve the agent's team for team-based filtering.
func userHasAgentAccess(user *auth.User, agentID string, store StoreReader) bool {
	if len(user.Teams) == 0 && len(user.Agents) == 0 {
		return false
	}
	for _, a := range user.Agents {
		if a == agentID {
			return true
		}
	}
	// Look up agent's team from the store for team-based access control.
	if len(user.Teams) > 0 && store != nil {
		agent := store.GetAgent(agentID)
		if agent != nil {
			for _, t := range user.Teams {
				if t == agent.Team {
					return true
				}
			}
		}
	}
	return false
}

// closeAll closes all connected clients.
func (h *wsHub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()

	for client := range h.clients {
		client.closeSend()
		client.conn.Close(websocket.StatusGoingAway, "server shutting down") //nolint:errcheck // best-effort websocket cleanup
		delete(h.clients, client)
	}
}

// clientCount returns the number of connected clients.
func (h *wsHub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}
