package director

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

// ToolDefinition is an alias for types.ToolDefinition.
type ToolDefinition = types.ToolDefinition

// ToolParam is an alias for types.ToolParam.
type ToolParam = types.ToolParam

// AuthFunc validates that a given sender (from the envelope's From field)
// is authorized to invoke director tools. Return nil to allow, or an error
// to deny the request.
type AuthFunc func(senderID string) error

// Director manages the org-level director agent. The director has visibility
// into all teams and agents, and can send messages, invoke capabilities,
// and query cluster status.
type Director struct {
	agentID  string
	natsConn *nats.Conn
	store    *state.Store
	logger   *slog.Logger
	authFunc AuthFunc

	mu   sync.Mutex
	subs []*nats.Subscription
}

// NewDirector creates a new Director instance. The optional authFn callback
// validates incoming requests by sender ID. If authFn is nil, requests are
// still validated for a non-empty sender identity (From field) but no
// additional authorization is performed.
func NewDirector(agentID string, nc *nats.Conn, store *state.Store, logger *slog.Logger, authFn AuthFunc) *Director {
	return &Director{
		agentID:  agentID,
		natsConn: nc,
		store:    store,
		logger:   logger,
		authFunc: authFn,
	}
}

// Start subscribes to NATS subjects for director tool invocations.
// Each tool is exposed on: hive.org.capabilities.director.{tool_name}.request
func (d *Director) Start() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	tools := map[string]func(*nats.Msg){
		"hive_list_teams":        d.handleListTeams,
		"hive_list_all_agents":   d.handleListAllAgents,
		"hive_message_lead":      d.handleMessageLead,
		"hive_message_agent":     d.handleMessageAgent,
		"hive_broadcast_leads":   d.handleBroadcastLeads,
		"hive_broadcast_all":     d.handleBroadcastAll,
		"hive_invoke_capability": d.handleInvokeCapability,
		"hive_team_status":       d.handleTeamStatus,
		"hive_cluster_status":    d.handleClusterStatus,
	}

	for name, handler := range tools {
		subject := fmt.Sprintf("hive.org.capabilities.director.%s.request", name)
		h := handler // capture for closure

		sub, err := d.natsConn.Subscribe(subject, d.withAuth(h))
		if err != nil {
			d.cleanup()
			return fmt.Errorf("subscribing to director tool %s: %w", subject, err)
		}
		d.subs = append(d.subs, sub)

		d.logger.Info("director tool registered",
			"tool", name,
			"subject", subject,
		)
	}

	d.logger.Info("director started", "agent_id", d.agentID, "tools", len(tools))
	return nil
}

// Stop unsubscribes from all director tool subjects.
func (d *Director) Stop() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.cleanup()
	d.logger.Info("director stopped", "agent_id", d.agentID)
}

// cleanup unsubscribes all subscriptions (caller must hold mu).
func (d *Director) cleanup() {
	for _, sub := range d.subs {
		if err := sub.Unsubscribe(); err != nil {
			d.logger.Warn("error unsubscribing director tool",
				"subject", sub.Subject,
				"error", err,
			)
		}
	}
	d.subs = nil
}

// GenerateTools returns the tool definitions available to the director agent.
func (d *Director) GenerateTools(teams map[string]*types.TeamManifest) []ToolDefinition {
	return []ToolDefinition{
		{
			Name:        "hive_list_teams",
			Description: "List all teams in the organization",
			Outputs: []ToolParam{
				{Name: "teams", Type: "array", Description: "List of team objects", Required: true},
			},
		},
		{
			Name:        "hive_list_all_agents",
			Description: "List all agents across all teams",
			Outputs: []ToolParam{
				{Name: "agents", Type: "array", Description: "List of agent objects", Required: true},
			},
		},
		{
			Name:        "hive_message_lead",
			Description: "Send a message to a team's lead agent",
			Inputs: []ToolParam{
				{Name: "team_id", Type: "string", Description: "The team ID", Required: true},
				{Name: "message", Type: "string", Description: "The message to send", Required: true},
			},
			Outputs: []ToolParam{
				{Name: "status", Type: "string", Description: "Delivery status", Required: true},
			},
		},
		{
			Name:        "hive_message_agent",
			Description: "Send a direct message to a specific agent",
			Inputs: []ToolParam{
				{Name: "agent_id", Type: "string", Description: "The agent ID", Required: true},
				{Name: "message", Type: "string", Description: "The message to send", Required: true},
			},
			Outputs: []ToolParam{
				{Name: "status", Type: "string", Description: "Delivery status", Required: true},
			},
		},
		{
			Name:        "hive_broadcast_leads",
			Description: "Broadcast a message to all team leads",
			Inputs: []ToolParam{
				{Name: "message", Type: "string", Description: "The message to broadcast", Required: true},
			},
			Outputs: []ToolParam{
				{Name: "status", Type: "string", Description: "Broadcast status", Required: true},
			},
		},
		{
			Name:        "hive_broadcast_all",
			Description: "Broadcast a message to all agents in the organization",
			Inputs: []ToolParam{
				{Name: "message", Type: "string", Description: "The message to broadcast", Required: true},
			},
			Outputs: []ToolParam{
				{Name: "status", Type: "string", Description: "Broadcast status", Required: true},
			},
		},
		{
			Name:        "hive_invoke_capability",
			Description: "Invoke a capability on a specific agent",
			Inputs: []ToolParam{
				{Name: "agent_id", Type: "string", Description: "The agent ID", Required: true},
				{Name: "capability", Type: "string", Description: "The capability name", Required: true},
				{Name: "inputs", Type: "object", Description: "Input parameters for the capability", Required: false},
			},
			Outputs: []ToolParam{
				{Name: "result", Type: "object", Description: "The capability invocation result", Required: true},
			},
		},
		{
			Name:        "hive_team_status",
			Description: "Get the status of a specific team and its agents",
			Inputs: []ToolParam{
				{Name: "team_id", Type: "string", Description: "The team ID", Required: true},
			},
			Outputs: []ToolParam{
				{Name: "status", Type: "object", Description: "Team status object", Required: true},
			},
		},
		{
			Name:        "hive_cluster_status",
			Description: "Get the overall cluster status including all teams and agents",
			Outputs: []ToolParam{
				{Name: "status", Type: "object", Description: "Cluster status object", Required: true},
			},
		},
	}
}

// --- Tool Handlers ---

func (d *Director) handleListTeams(msg *nats.Msg) {
	agents := d.store.AllAgents()

	// Build team set from agent team assignments.
	teamSet := make(map[string]int)
	for _, a := range agents {
		if a.Team != "" {
			teamSet[a.Team]++
		}
	}

	type teamInfo struct {
		ID         string `json:"id"`
		AgentCount int    `json:"agent_count"`
	}

	teams := make([]teamInfo, 0, len(teamSet))
	for id, count := range teamSet {
		teams = append(teams, teamInfo{ID: id, AgentCount: count})
	}

	d.respondJSON(msg, map[string]interface{}{
		"teams": teams,
	})
}

func (d *Director) handleListAllAgents(msg *nats.Msg) {
	agents := d.store.AllAgents()

	type agentInfo struct {
		ID     string `json:"id"`
		Team   string `json:"team"`
		Status string `json:"status"`
	}

	infos := make([]agentInfo, 0, len(agents))
	for _, a := range agents {
		infos = append(infos, agentInfo{
			ID:     a.ID,
			Team:   a.Team,
			Status: string(a.Status),
		})
	}

	d.respondJSON(msg, map[string]interface{}{
		"agents": infos,
	})
}

func (d *Director) handleMessageLead(msg *nats.Msg) {
	var req struct {
		TeamID  string `json:"team_id"`
		Message string `json:"message"`
	}
	if err := d.parseRequest(msg, &req); err != nil {
		d.respondError(msg, "INVALID_REQUEST", err.Error())
		return
	}

	if req.TeamID == "" || req.Message == "" {
		d.respondError(msg, "INVALID_REQUEST", "team_id and message are required")
		return
	}

	// Find the team's lead agent by looking for agents in the team.
	// In a full implementation, this would check the team manifest's lead field.
	// For now, publish to the team's lead subject.
	subject := fmt.Sprintf("hive.org.leads.%s", req.TeamID)

	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      d.agentID,
		To:        req.TeamID,
		Type:      types.MessageTypeTask,
		Timestamp: time.Now().UTC(),
		Payload:   map[string]string{"message": req.Message},
	}

	if err := env.Validate(); err != nil {
		d.logger.Warn("envelope validation failed before publishing message to lead",
			"team_id", req.TeamID,
			"error", err,
		)
	}

	data, err := json.Marshal(env)
	if err != nil {
		d.respondError(msg, "INTERNAL_ERROR", fmt.Sprintf("marshal error: %s", err))
		return
	}

	if err := d.natsConn.Publish(subject, data); err != nil {
		d.respondError(msg, "PUBLISH_ERROR", fmt.Sprintf("failed to message lead: %s", err))
		return
	}

	d.respondJSON(msg, map[string]interface{}{
		"status": "delivered",
	})
}

func (d *Director) handleMessageAgent(msg *nats.Msg) {
	var req struct {
		AgentID string `json:"agent_id"`
		Message string `json:"message"`
	}
	if err := d.parseRequest(msg, &req); err != nil {
		d.respondError(msg, "INVALID_REQUEST", err.Error())
		return
	}

	if req.AgentID == "" || req.Message == "" {
		d.respondError(msg, "INVALID_REQUEST", "agent_id and message are required")
		return
	}

	subject := fmt.Sprintf("hive.agent.%s.inbox", req.AgentID)

	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      d.agentID,
		To:        req.AgentID,
		Type:      types.MessageTypeTask,
		Timestamp: time.Now().UTC(),
		Payload:   map[string]string{"message": req.Message},
	}

	if err := env.Validate(); err != nil {
		d.logger.Warn("envelope validation failed before publishing message to agent",
			"agent_id", req.AgentID,
			"error", err,
		)
	}

	data, err := json.Marshal(env)
	if err != nil {
		d.respondError(msg, "INTERNAL_ERROR", fmt.Sprintf("marshal error: %s", err))
		return
	}

	if err := d.natsConn.Publish(subject, data); err != nil {
		d.respondError(msg, "PUBLISH_ERROR", fmt.Sprintf("failed to message agent: %s", err))
		return
	}

	d.respondJSON(msg, map[string]interface{}{
		"status": "delivered",
	})
}

func (d *Director) handleBroadcastLeads(msg *nats.Msg) {
	var req struct {
		Message string `json:"message"`
	}
	if err := d.parseRequest(msg, &req); err != nil {
		d.respondError(msg, "INVALID_REQUEST", err.Error())
		return
	}

	if req.Message == "" {
		d.respondError(msg, "INVALID_REQUEST", "message is required")
		return
	}

	subject := "hive.org.broadcast"

	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      d.agentID,
		To:        "broadcast",
		Type:      types.MessageTypeBroadcast,
		Timestamp: time.Now().UTC(),
		Payload:   map[string]string{"message": req.Message},
	}

	if err := env.Validate(); err != nil {
		d.logger.Warn("envelope validation failed before publishing broadcast to leads",
			"error", err,
		)
	}

	data, err := json.Marshal(env)
	if err != nil {
		d.respondError(msg, "INTERNAL_ERROR", fmt.Sprintf("marshal error: %s", err))
		return
	}

	if err := d.natsConn.Publish(subject, data); err != nil {
		d.respondError(msg, "PUBLISH_ERROR", fmt.Sprintf("failed to broadcast: %s", err))
		return
	}

	d.respondJSON(msg, map[string]interface{}{
		"status": "broadcast_sent",
	})
}

func (d *Director) handleBroadcastAll(msg *nats.Msg) {
	var req struct {
		Message string `json:"message"`
	}
	if err := d.parseRequest(msg, &req); err != nil {
		d.respondError(msg, "INVALID_REQUEST", err.Error())
		return
	}

	if req.Message == "" {
		d.respondError(msg, "INVALID_REQUEST", "message is required")
		return
	}

	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      d.agentID,
		To:        "broadcast",
		Type:      types.MessageTypeBroadcast,
		Timestamp: time.Now().UTC(),
		Payload:   map[string]string{"message": req.Message},
	}

	if err := env.Validate(); err != nil {
		d.logger.Warn("envelope validation failed before publishing broadcast to all",
			"error", err,
		)
	}

	data, err := json.Marshal(env)
	if err != nil {
		d.respondError(msg, "INTERNAL_ERROR", fmt.Sprintf("marshal error: %s", err))
		return
	}

	// Collect all unique teams from the agent store and publish to each
	// team's broadcast subject: hive.team.{TEAM_ID}.broadcast
	agents := d.store.AllAgents()
	teamSet := make(map[string]struct{})
	for _, a := range agents {
		if a.Team != "" {
			teamSet[a.Team] = struct{}{}
		}
	}

	if len(teamSet) == 0 {
		d.respondError(msg, "NO_TEAMS", "no teams found to broadcast to")
		return
	}

	var failed []string
	for teamID := range teamSet {
		subject := fmt.Sprintf("hive.team.%s.broadcast", teamID)
		if err := d.natsConn.Publish(subject, data); err != nil {
			d.logger.Error("failed to broadcast to team",
				"team_id", teamID,
				"subject", subject,
				"error", err,
			)
			failed = append(failed, teamID)
		}
	}

	if len(failed) > 0 {
		d.respondError(msg, "PARTIAL_BROADCAST",
			fmt.Sprintf("failed to broadcast to teams: %v", failed))
		return
	}

	d.respondJSON(msg, map[string]interface{}{
		"status":     "broadcast_sent",
		"team_count": len(teamSet),
	})
}

func (d *Director) handleInvokeCapability(msg *nats.Msg) {
	var req struct {
		AgentID    string                 `json:"agent_id"`
		Capability string                 `json:"capability"`
		Inputs     map[string]interface{} `json:"inputs"`
	}
	if err := d.parseRequest(msg, &req); err != nil {
		d.respondError(msg, "INVALID_REQUEST", err.Error())
		return
	}

	if req.AgentID == "" || req.Capability == "" {
		d.respondError(msg, "INVALID_REQUEST", "agent_id and capability are required")
		return
	}

	// Forward to the agent's capability subject.
	subject := fmt.Sprintf("hive.capabilities.%s.%s.request", req.AgentID, req.Capability)

	invokeReq := map[string]interface{}{
		"capability": req.Capability,
		"inputs":     req.Inputs,
	}

	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      d.agentID,
		To:        req.AgentID,
		Type:      types.MessageTypeCapabilityRequest,
		Timestamp: time.Now().UTC(),
		Payload:   invokeReq,
	}

	if err := env.Validate(); err != nil {
		d.logger.Warn("envelope validation failed before publishing capability request",
			"agent_id", req.AgentID,
			"capability", req.Capability,
			"error", err,
		)
	}

	data, err := json.Marshal(env)
	if err != nil {
		d.respondError(msg, "INTERNAL_ERROR", fmt.Sprintf("marshal error: %s", err))
		return
	}

	resp, err := d.natsConn.Request(subject, data, 30*time.Second)
	if err != nil {
		if err == nats.ErrTimeout {
			d.respondError(msg, "TIMEOUT", "capability invocation timed out")
			return
		}
		d.respondError(msg, "INVOKE_ERROR", fmt.Sprintf("invocation failed: %s", err))
		return
	}

	// Unmarshal the agent's response envelope to extract the capability
	// InvocationResponse payload. The agent's envelope has To: director-id,
	// so we cannot forward it raw — we must re-wrap it with correct From/To.
	var respEnv types.Envelope
	if err := json.Unmarshal(resp.Data, &respEnv); err != nil {
		d.respondError(msg, "INTERNAL_ERROR", fmt.Sprintf("failed to parse agent response: %s", err))
		return
	}

	d.respondJSON(msg, respEnv.Payload)
}

func (d *Director) handleTeamStatus(msg *nats.Msg) {
	var req struct {
		TeamID string `json:"team_id"`
	}
	if err := d.parseRequest(msg, &req); err != nil {
		d.respondError(msg, "INVALID_REQUEST", err.Error())
		return
	}

	if req.TeamID == "" {
		d.respondError(msg, "INVALID_REQUEST", "team_id is required")
		return
	}

	agents := d.store.AllAgents()

	type agentStatus struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}

	var teamAgents []agentStatus
	for _, a := range agents {
		if a.Team == req.TeamID {
			teamAgents = append(teamAgents, agentStatus{
				ID:     a.ID,
				Status: string(a.Status),
			})
		}
	}

	running := 0
	for _, a := range teamAgents {
		if a.Status == string(state.AgentStatusRunning) {
			running++
		}
	}

	d.respondJSON(msg, map[string]interface{}{
		"team_id":      req.TeamID,
		"agent_count":  len(teamAgents),
		"running":      running,
		"agents":       teamAgents,
	})
}

func (d *Director) handleClusterStatus(msg *nats.Msg) {
	agents := d.store.AllAgents()
	nodes := d.store.AllNodes()

	running := 0
	for _, a := range agents {
		if a.Status == state.AgentStatusRunning {
			running++
		}
	}

	onlineNodes := 0
	for _, n := range nodes {
		if n.Status == types.NodeStatusOnline {
			onlineNodes++
		}
	}

	// Build team summary.
	teamCounts := make(map[string]int)
	for _, a := range agents {
		if a.Team != "" {
			teamCounts[a.Team]++
		}
	}

	d.respondJSON(msg, map[string]interface{}{
		"total_agents": len(agents),
		"running":      running,
		"total_nodes":  len(nodes),
		"online_nodes": onlineNodes,
		"team_count":   len(teamCounts),
	})
}

// --- Auth ---

// withAuth wraps a NATS message handler with authentication. It always
// validates that the envelope can be parsed and contains a non-empty From
// field (baseline sender identity check). If authFunc is additionally
// configured, it is called to perform authorization. Unauthorized requests
// receive an error response.
func (d *Director) withAuth(handler func(*nats.Msg)) func(*nats.Msg) {
	return func(msg *nats.Msg) {
		var env types.Envelope
		if err := json.Unmarshal(msg.Data, &env); err != nil {
			d.respondError(msg, "INVALID_REQUEST", "failed to parse envelope")
			return
		}
		if env.From == "" {
			d.respondError(msg, "UNAUTHORIZED", "missing sender identity (From field)")
			return
		}
		if d.authFunc != nil {
			if err := d.authFunc(env.From); err != nil {
				d.logger.Warn("unauthorized director tool invocation",
					"from", env.From,
					"subject", msg.Subject,
					"error", err,
				)
				d.respondError(msg, "UNAUTHORIZED", fmt.Sprintf("not authorized: %s", err))
				return
			}
		}
		handler(msg)
	}
}

// --- Helper methods ---

// extractSender attempts to extract the From field from an incoming NATS message
// envelope. This is used to populate the To field in response envelopes. If the
// envelope cannot be parsed, it returns "unknown".
func (d *Director) extractSender(msg *nats.Msg) string {
	var env struct {
		From string `json:"from"`
	}
	if err := json.Unmarshal(msg.Data, &env); err == nil && env.From != "" {
		return env.From
	}
	return "unknown"
}

// extractRequestID attempts to extract the ID field from an incoming NATS
// message envelope. This is used to set CorrelationID on response envelopes
// so callers can correlate responses with their original requests. If the
// envelope cannot be parsed, it returns an empty string.
func (d *Director) extractRequestID(msg *nats.Msg) string {
	var env struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(msg.Data, &env); err == nil {
		return env.ID
	}
	return ""
}

// parseRequest extracts the payload from a NATS message envelope into the target struct.
func (d *Director) parseRequest(msg *nats.Msg, target interface{}) error {
	var env types.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		return fmt.Errorf("unmarshaling envelope: %w", err)
	}

	payloadBytes, err := json.Marshal(env.Payload)
	if err != nil {
		return fmt.Errorf("re-marshaling payload: %w", err)
	}

	if err := json.Unmarshal(payloadBytes, target); err != nil {
		return fmt.Errorf("unmarshaling payload: %w", err)
	}

	return nil
}

// respondJSON sends a JSON response envelope on the reply subject.
func (d *Director) respondJSON(msg *nats.Msg, payload interface{}) {
	if msg.Reply == "" {
		return
	}

	// Extract the sender and request ID from the incoming envelope.
	to := d.extractSender(msg)
	requestID := d.extractRequestID(msg)

	env := types.Envelope{
		ID:            types.NewUUID(),
		From:          d.agentID,
		To:            to,
		Type:          types.MessageTypeResult,
		Timestamp:     time.Now().UTC(),
		Payload:       payload,
		CorrelationID: requestID,
	}

	if err := env.Validate(); err != nil {
		d.logger.Warn("envelope validation failed before publishing response",
			"to", to,
			"error", err,
		)
	}

	data, err := json.Marshal(env)
	if err != nil {
		d.logger.Error("failed to marshal response", "error", err)
		return
	}

	if err := d.natsConn.Publish(msg.Reply, data); err != nil {
		d.logger.Error("failed to publish response", "reply", msg.Reply, "error", err)
	}
}

// respondError sends an error response envelope on the reply subject.
func (d *Director) respondError(msg *nats.Msg, code, message string) {
	if msg.Reply == "" {
		return
	}

	// Extract the sender and request ID from the incoming envelope.
	to := d.extractSender(msg)
	requestID := d.extractRequestID(msg)

	env := types.Envelope{
		ID:            types.NewUUID(),
		From:          d.agentID,
		To:            to,
		Type:          types.MessageTypeError,
		Timestamp:     time.Now().UTC(),
		CorrelationID: requestID,
		Payload: map[string]string{
			"code":    code,
			"message": message,
		},
	}

	if err := env.Validate(); err != nil {
		d.logger.Warn("envelope validation failed before publishing error response",
			"to", to,
			"error", err,
		)
	}

	data, err := json.Marshal(env)
	if err != nil {
		d.logger.Error("failed to marshal error response", "error", err)
		return
	}

	if err := d.natsConn.Publish(msg.Reply, data); err != nil {
		d.logger.Error("failed to publish error response", "reply", msg.Reply, "error", err)
	}
}

