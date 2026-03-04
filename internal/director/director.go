package director

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

// ToolDefinition describes a tool available to the director agent.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Inputs      []ToolParam     `json:"inputs,omitempty"`
	Outputs     []ToolParam     `json:"outputs,omitempty"`
}

// ToolParam describes a parameter for a tool.
type ToolParam struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
}

// Director manages the org-level director agent. The director has visibility
// into all teams and agents, and can send messages, invoke capabilities,
// and query cluster status.
type Director struct {
	agentID  string
	natsConn *nats.Conn
	store    *state.Store
	logger   *slog.Logger

	mu   sync.Mutex
	subs []*nats.Subscription
}

// NewDirector creates a new Director instance.
func NewDirector(agentID string, nc *nats.Conn, store *state.Store, logger *slog.Logger) *Director {
	return &Director{
		agentID:  agentID,
		natsConn: nc,
		store:    store,
		logger:   logger,
	}
}

// Start subscribes to NATS subjects for director tool invocations.
// Each tool is exposed on: hive.director.{tool_name}.request
func (d *Director) Start() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	tools := map[string]func(*nats.Msg){
		"hive_list_teams":       d.handleListTeams,
		"hive_list_all_agents":  d.handleListAllAgents,
		"hive_message_lead":     d.handleMessageLead,
		"hive_message_agent":    d.handleMessageAgent,
		"hive_broadcast_leads":  d.handleBroadcastLeads,
		"hive_broadcast_all":    d.handleBroadcastAll,
		"hive_invoke_capability": d.handleInvokeCapability,
		"hive_team_status":      d.handleTeamStatus,
		"hive_cluster_status":   d.handleClusterStatus,
	}

	for name, handler := range tools {
		subject := fmt.Sprintf("hive.director.%s.request", name)
		h := handler // capture for closure

		sub, err := d.natsConn.Subscribe(subject, h)
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
	subject := fmt.Sprintf("hive.team.%s.lead", req.TeamID)

	env := types.Envelope{
		ID:        newUUID(),
		From:      d.agentID,
		To:        req.TeamID,
		Type:      types.MessageTypeTask,
		Timestamp: time.Now().UTC(),
		Payload:   map[string]string{"message": req.Message},
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
		ID:        newUUID(),
		From:      d.agentID,
		To:        req.AgentID,
		Type:      types.MessageTypeTask,
		Timestamp: time.Now().UTC(),
		Payload:   map[string]string{"message": req.Message},
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

	subject := "hive.broadcast.leads"

	env := types.Envelope{
		ID:        newUUID(),
		From:      d.agentID,
		Type:      types.MessageTypeBroadcast,
		Timestamp: time.Now().UTC(),
		Payload:   map[string]string{"message": req.Message},
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

	subject := "hive.broadcast.all"

	env := types.Envelope{
		ID:        newUUID(),
		From:      d.agentID,
		Type:      types.MessageTypeBroadcast,
		Timestamp: time.Now().UTC(),
		Payload:   map[string]string{"message": req.Message},
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
		ID:        newUUID(),
		From:      d.agentID,
		To:        req.AgentID,
		Type:      types.MessageTypeCapabilityRequest,
		Timestamp: time.Now().UTC(),
		Payload:   invokeReq,
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

	// Forward the response directly.
	if msg.Reply != "" {
		if err := d.natsConn.Publish(msg.Reply, resp.Data); err != nil {
			d.logger.Error("failed to publish invoke response", "error", err)
		}
	}
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

// --- Helper methods ---

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

	env := types.Envelope{
		ID:        newUUID(),
		From:      d.agentID,
		Type:      types.MessageTypeResult,
		Timestamp: time.Now().UTC(),
		Payload:   payload,
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

	env := types.Envelope{
		ID:        newUUID(),
		From:      d.agentID,
		Type:      types.MessageTypeError,
		Timestamp: time.Now().UTC(),
		Payload: map[string]string{
			"code":    code,
			"message": message,
		},
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
