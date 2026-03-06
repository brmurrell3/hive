package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivehq/hive/internal/config"
	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// DaemonClient communicates with a running hived instance via NATS.
// Instead of creating independent state stores and VM managers, it sends
// control messages to hived which owns the actual state and VM lifecycle.
type DaemonClient struct {
	nc *nats.Conn
}

// CtlRequest is the payload sent from hivectl to hived on control subjects.
type CtlRequest struct {
	AgentID string `json:"agent_id"`
}

// CtlResponse is the payload returned from hived to hivectl on control subjects.
type CtlResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`

	// Used by status responses.
	Agent *state.AgentState `json:"agent,omitempty"`

	// Used by list responses.
	Agents []*state.AgentState `json:"agents,omitempty"`
}

// natsURLFromConfig reads cluster.yaml and derives the NATS client URL.
// It uses the configured host (defaulting to 127.0.0.1) and port.
func natsURLFromConfig(root string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolving cluster root: %w", err)
	}

	cfg, err := config.LoadCluster(absRoot)
	if err != nil {
		return "", fmt.Errorf("loading cluster config: %w", err)
	}

	host := cfg.Spec.NATS.Host
	if host == "" {
		host = "127.0.0.1"
	}

	return fmt.Sprintf("nats://%s:%d", host, cfg.Spec.NATS.Port), nil
}

// natsAuthToken reads the NATS auth token from .state/nats-auth-token.
// If the cluster config has an explicit authToken set, that is returned instead.
// Returns an empty string (no auth) if neither source is available.
func natsAuthToken(root string) string {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return ""
	}

	// Check cluster.yaml first — operator may have set a static token.
	cfg, err := config.LoadCluster(absRoot)
	if err == nil && cfg.Spec.NATS.AuthToken != "" {
		return cfg.Spec.NATS.AuthToken
	}

	// Fall back to the runtime-generated token written by hived on startup.
	tokenPath := filepath.Join(absRoot, ".state", "nats-auth-token")
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// newDaemonClient connects to hived's NATS server using the port from cluster.yaml
// and authenticates with the token from .state/nats-auth-token (or cluster.yaml).
func newDaemonClient() (*DaemonClient, error) {
	natsURL, err := natsURLFromConfig(clusterRoot)
	if err != nil {
		return nil, fmt.Errorf("determining NATS URL: %w", err)
	}

	opts := []nats.Option{
		nats.Timeout(5 * time.Second),
		nats.Name("hivectl"),
	}

	if token := natsAuthToken(clusterRoot); token != "" {
		opts = append(opts, nats.Token(token))
	}

	nc, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to hived NATS at %s: %w (is hived running?)", natsURL, err)
	}

	return &DaemonClient{nc: nc}, nil
}

// Close drains and closes the NATS connection.
func (c *DaemonClient) Close() {
	if c.nc != nil {
		c.nc.Drain()
	}
}

// request sends a control request to hived and waits for a response.
func (c *DaemonClient) request(subject string, req interface{}) (*CtlResponse, error) {
	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      "hivectl",
		To:        "hived",
		Type:      types.MessageTypeControl,
		Timestamp: time.Now(),
		Payload:   req,
	}

	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	msg, err := c.nc.Request(subject, data, 30*time.Second)
	if err != nil {
		if err == nats.ErrTimeout {
			return nil, fmt.Errorf("request timed out: hived may not be running or is unresponsive")
		}
		return nil, fmt.Errorf("sending request: %w", err)
	}

	var resp CtlResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	return &resp, nil
}

// StartAgent sends a start request to hived for the given agent ID.
func (c *DaemonClient) StartAgent(agentID string) error {
	resp, err := c.request("hive.ctl.agents.start", CtlRequest{AgentID: agentID})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

// StopAgent sends a stop request to hived for the given agent ID.
func (c *DaemonClient) StopAgent(agentID string) error {
	resp, err := c.request("hive.ctl.agents.stop", CtlRequest{AgentID: agentID})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

// RestartAgent sends a restart request to hived for the given agent ID.
func (c *DaemonClient) RestartAgent(agentID string) error {
	resp, err := c.request("hive.ctl.agents.restart", CtlRequest{AgentID: agentID})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

// DestroyAgent sends a destroy request to hived for the given agent ID.
func (c *DaemonClient) DestroyAgent(agentID string) error {
	resp, err := c.request("hive.ctl.agents.destroy", CtlRequest{AgentID: agentID})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

// AgentStatus requests the status of a single agent from hived.
func (c *DaemonClient) AgentStatus(agentID string) (*state.AgentState, error) {
	resp, err := c.request("hive.ctl.agents.status", CtlRequest{AgentID: agentID})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	return resp.Agent, nil
}

// AgentList requests the list of all agents from hived.
func (c *DaemonClient) AgentList() ([]*state.AgentState, error) {
	resp, err := c.request("hive.ctl.agents.list", CtlRequest{})
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("%s", resp.Error)
	}
	return resp.Agents, nil
}
