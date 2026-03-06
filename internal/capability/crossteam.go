package capability

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// exposedCapability tracks a capability exposed cross-team by a specific team.
type exposedCapability struct {
	teamID         string
	agentID        string
	capability     string
	allowedCallers []string // team IDs allowed to invoke; empty means all teams
}

// CrossTeamRouter manages cross-team capability exposure and invocation.
// It reads each team's crossTeamCapabilities configuration and sets up
// NATS subscriptions on org-level capability subjects, forwarding requests
// to the appropriate agent's capability handler.
type CrossTeamRouter struct {
	natsConn *nats.Conn
	store    *state.Store
	logger   *slog.Logger

	mu      sync.Mutex
	subs    []*nats.Subscription
	exposed map[string]*exposedCapability // "agentID.capability" -> exposed info
}

// NewCrossTeamRouter creates a new cross-team capability router.
func NewCrossTeamRouter(nc *nats.Conn, store *state.Store, logger *slog.Logger) *CrossTeamRouter {
	return &CrossTeamRouter{
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
//	hive.org.capabilities.{AGENT_ID}.{CAPABILITY}.request
//
// The router forwards requests to the agent's internal capability subject:
//
//	hive.capabilities.{AGENT_ID}.{CAPABILITY}.request
func (r *CrossTeamRouter) Start(teams map[string]*types.TeamManifest) error {
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
					teamID:         teamID,
					agentID:        agentID,
					capability:     agentCap.Name,
					allowedCallers: team.Spec.Communication.AllowedCallers,
				}

				subject := fmt.Sprintf("hive.org.capabilities.%s.%s.request", agentID, agentCap.Name)
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
func (r *CrossTeamRouter) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.cleanup()
	r.logger.Info("cross-team router stopped")
}

// cleanup unsubscribes all subscriptions (caller must hold mu).
func (r *CrossTeamRouter) cleanup() {
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
func (r *CrossTeamRouter) IsExposed(agentID, capability string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := fmt.Sprintf("%s.%s", agentID, capability)
	_, ok := r.exposed[key]
	return ok
}

// handleCrossTeamRequest forwards a cross-team request to the agent's internal
// capability handler subject.
func (r *CrossTeamRouter) handleCrossTeamRequest(msg *nats.Msg, agentID, capName string) {
	start := time.Now()

	// Parse the envelope to validate caller identity.
	var env types.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		r.publishErrorResponse(msg, "unknown", capName, "INVALID_REQUEST",
			"failed to parse request envelope", start)
		return
	}

	if env.From == "" {
		r.publishErrorResponse(msg, "unknown", capName, "UNAUTHORIZED",
			"missing sender identity (From field) in cross-team request", start)
		return
	}

	r.logger.Info("received cross-team capability request",
		"agent_id", agentID,
		"capability", capName,
		"subject", msg.Subject,
		"from", env.From,
	)

	// Verify the capability is still exposed.
	key := fmt.Sprintf("%s.%s", agentID, capName)
	r.mu.Lock()
	cap, exposed := r.exposed[key]
	r.mu.Unlock()

	if !exposed {
		r.publishErrorResponse(msg, env.From, capName, "PERMISSION_DENIED",
			fmt.Sprintf("capability %q on agent %q is not exposed cross-team", capName, agentID), start)
		return
	}

	// Check allowed callers if configured.
	if len(cap.allowedCallers) > 0 {
		callerTeam := r.resolveCallerTeam(env.From)
		if !isCallerAllowed(callerTeam, cap.allowedCallers) {
			r.logger.Warn("cross-team request denied: caller not in allowedCallers",
				"from", env.From,
				"caller_team", callerTeam,
				"allowed_callers", cap.allowedCallers,
				"capability", capName,
				"agent_id", agentID,
			)
			r.publishErrorResponse(msg, env.From, capName, "PERMISSION_DENIED",
				fmt.Sprintf("team %q is not authorized to invoke capability %q on agent %q", callerTeam, capName, agentID), start)
			return
		}
	}

	// Forward to the agent's internal capability subject.
	internalSubject := fmt.Sprintf("hive.capabilities.%s.%s.request", agentID, capName)

	resp, err := r.natsConn.Request(internalSubject, msg.Data, 30*time.Second)
	if err != nil {
		if err == nats.ErrTimeout {
			r.publishErrorResponse(msg, env.From, capName, "TIMEOUT",
				fmt.Sprintf("capability %q on agent %q timed out", capName, agentID), start)
			return
		}
		r.publishErrorResponse(msg, env.From, capName, "FORWARD_ERROR",
			fmt.Sprintf("failed to forward to agent %q: %s", agentID, err.Error()), start)
		return
	}

	// Forward the raw response bytes back to the original caller.
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

// resolveCallerTeam looks up the team ID of a caller agent from the capability
// registry. Returns the team ID if found, or an empty string if unknown.
func (r *CrossTeamRouter) resolveCallerTeam(agentID string) string {
	registry := r.store.GetCapabilityRegistry()
	if entry, ok := registry.Agents[agentID]; ok {
		return entry.TeamID
	}
	return ""
}

// isCallerAllowed checks whether a caller's team is in the allowed callers list.
// An empty callerTeam is never allowed when allowedCallers is configured.
func isCallerAllowed(callerTeam string, allowedCallers []string) bool {
	if callerTeam == "" {
		return false
	}
	for _, allowed := range allowedCallers {
		if allowed == "*" || allowed == callerTeam {
			return true
		}
	}
	return false
}

// publishErrorResponse sends an error response to the reply subject.
func (r *CrossTeamRouter) publishErrorResponse(msg *nats.Msg, replyTo, capName, code, message string, start time.Time) {
	if msg.Reply == "" {
		return
	}

	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      "crossteam-router",
		To:        replyTo,
		Type:      types.MessageTypeCapabilityResponse,
		Timestamp: time.Now().UTC(),
		Payload: InvocationResponse{
			Capability: capName,
			Status:     "error",
			Error: &InvocationError{
				Code:      code,
				Message:   message,
				Retryable: code == "TIMEOUT",
			},
			DurationMs: time.Since(start).Milliseconds(),
		},
	}

	if err := env.Validate(); err != nil {
		r.logger.Warn("envelope validation failed before publishing error response",
			"capability", capName,
			"error", err,
		)
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

// CrossTeamToolName returns the cross-team tool name for a capability.
// Format: {CAPABILITY}-{TEAM_ID}
func CrossTeamToolName(capability, teamID string) string {
	return fmt.Sprintf("%s-%s", capability, teamID)
}
