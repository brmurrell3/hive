package crossteam

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// InvocationRequest is the payload for a cross-team capability request.
type InvocationRequest struct {
	Capability string                 `json:"capability"`
	Inputs     map[string]interface{} `json:"inputs"`
	Timeout    string                 `json:"timeout,omitempty"`
}

// InvocationResponse is the payload for a cross-team capability response.
type InvocationResponse struct {
	Capability string                 `json:"capability"`
	Status     string                 `json:"status"` // success, error, timeout
	Outputs    map[string]interface{} `json:"outputs,omitempty"`
	Error      *InvocationError       `json:"error,omitempty"`
	DurationMs int64                  `json:"duration_ms"`
}

// InvocationError describes an error during cross-team capability invocation.
type InvocationError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// exposedCapability tracks a capability exposed cross-team by a specific team.
type exposedCapability struct {
	teamID     string
	agentID    string
	capability string
}

// Router manages cross-team capability exposure and invocation.
// It reads each team's crossTeamCapabilities configuration and sets up
// NATS subscriptions on org-level capability subjects, forwarding requests
// to the appropriate agent's capability handler.
type Router struct {
	natsConn *nats.Conn
	store    *state.Store
	logger   *slog.Logger

	mu       sync.Mutex
	subs     []*nats.Subscription
	exposed  map[string]*exposedCapability // "agentID.capability" -> exposed info
}

// NewRouter creates a new cross-team capability Router.
func NewRouter(nc *nats.Conn, store *state.Store, logger *slog.Logger) *Router {
	return &Router{
		natsConn: nc,
		store:    store,
		logger:   logger,
		exposed:  make(map[string]*exposedCapability),
	}
}

// Start reads team manifests to determine exposed capabilities and subscribes
// to org-level NATS subjects for cross-team invocation.
//
// For each exposed capability, it subscribes to:
//
//	org.capabilities.{AGENT_ID}.{CAPABILITY}.request
//
// The router forwards requests to the agent's internal capability subject:
//
//	hive.capabilities.{AGENT_ID}.{CAPABILITY}.request
func (r *Router) Start(teams map[string]*types.TeamManifest) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Build the set of exposed capabilities from team manifests.
	registry := r.store.GetCapabilityRegistry()

	for teamID, team := range teams {
		exposedCaps := parseCrossTeamCapabilities(team.Spec.Communication.CrossTeamCapabilities)
		if len(exposedCaps) == 0 {
			continue
		}

		// Find agents in this team from the capability registry.
		for agentID, entry := range registry.Agents {
			if entry.TeamID != teamID {
				continue
			}

			for _, agentCap := range entry.Capabilities {
				if !isCapabilityExposed(agentCap.Name, exposedCaps) {
					continue
				}

				key := fmt.Sprintf("%s.%s", agentID, agentCap.Name)
				r.exposed[key] = &exposedCapability{
					teamID:     teamID,
					agentID:    agentID,
					capability: agentCap.Name,
				}

				subject := fmt.Sprintf("org.capabilities.%s.%s.request", agentID, agentCap.Name)
				capName := agentCap.Name
				aID := agentID

				sub, err := r.natsConn.Subscribe(subject, func(msg *nats.Msg) {
					r.handleCrossTeamRequest(msg, aID, capName)
				})
				if err != nil {
					r.cleanup()
					return fmt.Errorf("subscribing to cross-team subject %s: %w", subject, err)
				}

				r.subs = append(r.subs, sub)
				r.logger.Info("cross-team capability exposed",
					"team_id", teamID,
					"agent_id", agentID,
					"capability", agentCap.Name,
					"subject", subject,
				)
			}
		}
	}

	r.logger.Info("cross-team router started", "exposed_count", len(r.exposed))
	return nil
}

// Stop unsubscribes from all cross-team capability subjects.
func (r *Router) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.cleanup()
	r.logger.Info("cross-team router stopped")
}

// cleanup unsubscribes all subscriptions (caller must hold mu).
func (r *Router) cleanup() {
	for _, sub := range r.subs {
		if err := sub.Unsubscribe(); err != nil {
			r.logger.Warn("error unsubscribing cross-team subject",
				"subject", sub.Subject,
				"error", err,
			)
		}
	}
	r.subs = nil
	r.exposed = make(map[string]*exposedCapability)
}

// IsExposed returns whether a specific agent capability is exposed cross-team.
func (r *Router) IsExposed(agentID, capability string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := fmt.Sprintf("%s.%s", agentID, capability)
	_, ok := r.exposed[key]
	return ok
}

// handleCrossTeamRequest forwards a cross-team request to the agent's internal
// capability handler subject.
func (r *Router) handleCrossTeamRequest(msg *nats.Msg, agentID, capName string) {
	start := time.Now()

	r.logger.Info("received cross-team capability request",
		"agent_id", agentID,
		"capability", capName,
		"subject", msg.Subject,
	)

	// Verify the capability is still exposed.
	key := fmt.Sprintf("%s.%s", agentID, capName)
	r.mu.Lock()
	_, exposed := r.exposed[key]
	r.mu.Unlock()

	if !exposed {
		r.publishErrorResponse(msg, capName, "PERMISSION_DENIED",
			fmt.Sprintf("capability %q on agent %q is not exposed cross-team", capName, agentID), start)
		return
	}

	// Forward to the agent's internal capability subject.
	internalSubject := fmt.Sprintf("hive.capabilities.%s.%s.request", agentID, capName)

	resp, err := r.natsConn.Request(internalSubject, msg.Data, 30*time.Second)
	if err != nil {
		if err == nats.ErrTimeout {
			r.publishErrorResponse(msg, capName, "TIMEOUT",
				fmt.Sprintf("capability %q on agent %q timed out", capName, agentID), start)
			return
		}
		r.publishErrorResponse(msg, capName, "FORWARD_ERROR",
			fmt.Sprintf("failed to forward to agent %q: %s", agentID, err.Error()), start)
		return
	}

	// Forward the response back to the original caller.
	if msg.Reply != "" {
		if err := r.natsConn.Publish(msg.Reply, resp.Data); err != nil {
			r.logger.Error("failed to publish cross-team response",
				"agent_id", agentID,
				"capability", capName,
				"error", err,
			)
		}
	}

	r.logger.Info("cross-team capability request forwarded",
		"agent_id", agentID,
		"capability", capName,
		"duration_ms", time.Since(start).Milliseconds(),
	)
}

// publishErrorResponse sends an error response to the reply subject.
func (r *Router) publishErrorResponse(msg *nats.Msg, capName, code, message string, start time.Time) {
	if msg.Reply == "" {
		return
	}

	resp := InvocationResponse{
		Capability: capName,
		Status:     "error",
		Error: &InvocationError{
			Code:      code,
			Message:   message,
			Retryable: code == "TIMEOUT",
		},
		DurationMs: time.Since(start).Milliseconds(),
	}

	env := types.Envelope{
		ID:        newUUID(),
		From:      "crossteam-router",
		Type:      types.MessageTypeCapabilityResponse,
		Timestamp: time.Now().UTC(),
		Payload:   resp,
	}

	data, err := json.Marshal(env)
	if err != nil {
		r.logger.Error("failed to marshal cross-team error response", "error", err)
		return
	}

	if err := r.natsConn.Publish(msg.Reply, data); err != nil {
		r.logger.Error("failed to publish cross-team error response",
			"reply", msg.Reply,
			"error", err,
		)
	}
}

// parseCrossTeamCapabilities extracts the list of exposed capability names
// from the crossTeamCapabilities field, which can be:
//   - "all" (string): expose all capabilities
//   - []interface{}: list of capability name strings
//   - nil: no capabilities exposed
func parseCrossTeamCapabilities(v interface{}) []string {
	if v == nil {
		return nil
	}

	// String "all" means all capabilities are exposed.
	if s, ok := v.(string); ok && s == "all" {
		return []string{"*"}
	}

	// List of capability names.
	if list, ok := v.([]interface{}); ok {
		caps := make([]string, 0, len(list))
		for _, item := range list {
			if s, ok := item.(string); ok {
				caps = append(caps, s)
			}
		}
		return caps
	}

	return nil
}

// isCapabilityExposed checks if a capability name is in the exposed list.
// A wildcard "*" means all capabilities are exposed.
func isCapabilityExposed(name string, exposed []string) bool {
	for _, e := range exposed {
		if e == "*" || e == name {
			return true
		}
	}
	return false
}

// ToolName returns the cross-team tool name for a capability.
// Format: {CAPABILITY}-{TEAM_ID}
func ToolName(capability, teamID string) string {
	return fmt.Sprintf("%s-%s", capability, teamID)
}

// newUUID generates a UUID v4 string using crypto/rand.
func newUUID() string {
	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		return "00000000-0000-0000-0000-000000000000"
	}

	uuid[6] = (uuid[6] & 0x0f) | 0x40
	uuid[8] = (uuid[8] & 0x3f) | 0x80

	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uuid[0:4],
		uuid[4:6],
		uuid[6:8],
		uuid[8:10],
		uuid[10:16],
	)
}
