// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/config"
	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// NATS timeout constants used across hivectl commands.
const (
	// defaultNATSTimeout is the standard timeout for NATS requests
	// (connection, quick queries, dashboard fetches).
	defaultNATSTimeout = 5 * time.Second

	// daemonRequestTimeout is the timeout for DaemonClient control
	// requests that may involve slower operations like starting or
	// stopping agents.
	daemonRequestTimeout = 30 * time.Second
)

// DaemonClient communicates with a running hived instance via NATS.
// Instead of creating independent state stores and VM managers, it sends
// control messages to hived which owns the actual state and VM lifecycle.
type DaemonClient struct {
	nc *nats.Conn
}

// cachedConfig provides lazy loading of the cluster config so that
// natsURLFromConfig and natsAuthToken don't re-parse cluster.yaml independently.
// The config is reloaded if the root parameter changes between calls.
var (
	cachedCfgMu  sync.Mutex
	cachedCfg    *types.ClusterConfig
	cachedCfgErr error
	cachedRoot   string
)

func loadCachedConfig(root string) (*types.ClusterConfig, error) {
	cachedCfgMu.Lock()
	defer cachedCfgMu.Unlock()
	if cachedCfg != nil && cachedRoot == root && cachedCfgErr == nil {
		return cachedCfg, nil
	}
	cachedRoot = root
	absRoot, err := filepath.Abs(root)
	if err != nil {
		cachedCfgErr = fmt.Errorf("resolving cluster root: %w", err)
		cachedCfg = nil
		return nil, cachedCfgErr
	}
	cachedCfg, cachedCfgErr = config.LoadCluster(absRoot)
	return cachedCfg, cachedCfgErr
}

// natsURLFromConfig reads cluster.yaml and derives the NATS client URL.
// It uses the configured host (defaulting to 127.0.0.1) and port.
func natsURLFromConfig(root string) (string, error) {
	cfg, err := loadCachedConfig(root)
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
		slog.Warn("failed to load NATS auth token", "error", err)
		return ""
	}

	// Check cluster.yaml first -- operator may have set a static token.
	cfg, err := loadCachedConfig(root)
	if err == nil && cfg.Spec.NATS.AuthToken != "" {
		return cfg.Spec.NATS.AuthToken
	}

	// Fall back to the runtime-generated token written by hived on startup.
	tokenPath := filepath.Join(absRoot, ".state", "nats-auth-token")
	data, err := os.ReadFile(tokenPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			slog.Warn("NATS auth token file not found (is hived running?)", "path", tokenPath)
		} else {
			slog.Warn("failed to read NATS auth token file", "path", tokenPath, "error", err)
		}
		return ""
	}
	return strings.TrimSpace(string(data))
}

// connectNATS connects to hived's NATS server using cluster.yaml config and
// the auth token from .state/nats-auth-token (or cluster.yaml). The name
// parameter is used as the NATS client name for debugging.
func connectNATS(name string) (*nats.Conn, error) {
	natsURL, err := natsURLFromConfig(clusterRoot)
	if err != nil {
		return nil, fmt.Errorf("determining NATS URL: %w", err)
	}

	opts := []nats.Option{
		nats.Timeout(defaultNATSTimeout),
		nats.Name(name),
	}

	if token := natsAuthToken(clusterRoot); token != "" {
		opts = append(opts, nats.Token(token))
	}

	nc, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("connecting to NATS at %s: %w (is hived running?)", natsURL, err)
	}

	return nc, nil
}

// newDaemonClient connects to hived's NATS server using the port from cluster.yaml
// and authenticates with the token from .state/nats-auth-token (or cluster.yaml).
func newDaemonClient() (*DaemonClient, error) {
	nc, err := connectNATS("hivectl")
	if err != nil {
		return nil, err
	}
	return &DaemonClient{nc: nc}, nil
}

// Close drains and closes the NATS connection, waiting for pending
// messages to be flushed before returning.
func (c *DaemonClient) Close() {
	if c.nc != nil {
		if err := c.nc.Drain(); err != nil {
			fmt.Fprintf(os.Stderr, "hivectl: warning: NATS drain failed: %v\n", err)
		}
	}
}

// request sends a control request to hived and waits for a response.
func (c *DaemonClient) request(subject string, req interface{}) (*protocol.CtlResponse, error) {
	payloadBytes, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling request payload: %w", err)
	}

	env := types.Envelope{
		ID:        types.NewUUID(),
		From:      "hivectl",
		To:        "hived",
		Type:      types.MessageTypeControl,
		Timestamp: time.Now().UTC(),
		Payload:   payloadBytes,
		UserToken: authToken,
	}

	data, err := json.Marshal(env)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	msg, err := c.nc.Request(subject, data, daemonRequestTimeout)
	if err != nil {
		if errors.Is(err, nats.ErrTimeout) {
			return nil, fmt.Errorf("request timed out: hived may not be running or is unresponsive")
		}
		return nil, fmt.Errorf("sending request: %w", err)
	}

	var resp protocol.CtlResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	return &resp, nil
}

// agentAction sends an agent control request and returns an error on failure.
func (c *DaemonClient) agentAction(subject, agentID string) error {
	resp, err := c.request(subject, protocol.CtlRequest{AgentID: agentID})
	if err != nil {
		return err
	}
	return resp.Err()
}

// StartAgent sends a start request to hived for the given agent ID.
func (c *DaemonClient) StartAgent(agentID string) error {
	return c.agentAction(protocol.SubjAgentStart, agentID)
}

// StopAgent sends a stop request to hived for the given agent ID.
func (c *DaemonClient) StopAgent(agentID string) error {
	return c.agentAction(protocol.SubjAgentStop, agentID)
}

// RestartAgent sends a restart request to hived for the given agent ID.
func (c *DaemonClient) RestartAgent(agentID string) error {
	return c.agentAction(protocol.SubjAgentRestart, agentID)
}

// DestroyAgent sends a destroy request to hived for the given agent ID.
func (c *DaemonClient) DestroyAgent(agentID string) error {
	return c.agentAction(protocol.SubjAgentDestroy, agentID)
}

// AgentStatus requests the status of a single agent from hived.
func (c *DaemonClient) AgentStatus(agentID string) (*state.AgentState, error) {
	resp, err := c.request(protocol.SubjAgentStatus, protocol.CtlRequest{AgentID: agentID})
	if err != nil {
		return nil, err
	}
	if err := resp.Err(); err != nil {
		return nil, err
	}
	return resp.Agent, nil
}

// AgentList requests the list of all agents from hived.
func (c *DaemonClient) AgentList() ([]*state.AgentState, error) {
	resp, err := c.request(protocol.SubjAgentList, protocol.CtlRequest{})
	if err != nil {
		return nil, err
	}
	if err := resp.Err(); err != nil {
		return nil, err
	}
	return resp.Agents, nil
}
