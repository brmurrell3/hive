package node

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// Registry manages node registration and lifecycle.
type Registry struct {
	store    *state.Store
	natsConn *nats.Conn
	logger   *slog.Logger
	sub      *nats.Subscription
}

// NewRegistry creates a new node registry.
func NewRegistry(store *state.Store, nc *nats.Conn, logger *slog.Logger) *Registry {
	return &Registry{
		store:    store,
		natsConn: nc,
		logger:   logger.With("component", "node-registry"),
	}
}

// Start subscribes to join requests on NATS.
func (r *Registry) Start() error {
	sub, err := r.natsConn.Subscribe("hive.join.request", func(msg *nats.Msg) {
		r.handleJoinRequest(msg)
	})
	if err != nil {
		return fmt.Errorf("subscribing to join requests: %w", err)
	}
	r.sub = sub
	r.logger.Info("node registry started, listening for join requests")
	return nil
}

// Stop unsubscribes from NATS.
func (r *Registry) Stop() error {
	if r.sub != nil {
		return r.sub.Unsubscribe()
	}
	return nil
}

// handleJoinRequest processes a join request from a node.
func (r *Registry) handleJoinRequest(msg *nats.Msg) {
	var req types.JoinRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		r.logger.Warn("invalid join request", "error", err)
		r.respondToJoin(msg, types.JoinResponse{
			Accepted: false,
			Error:    "invalid request format",
		})
		return
	}

	// Validate token
	token := r.store.ValidateToken(req.Token)
	if token == nil {
		r.logger.Warn("join request with invalid token", "hostname", req.Hostname)
		r.respondToJoin(msg, types.JoinResponse{
			Accepted: false,
			Error:    "invalid or expired token",
		})
		return
	}

	// Classify tier
	tier := types.ClassifyTier(req.Resources)

	// Generate node ID
	nodeID := generateNodeID(req.Hostname, req.Arch)

	// Build labels
	labels := map[string]string{
		"hive.io/arch": req.Arch,
		"hive.io/tier": fmt.Sprintf("%d", tier),
		"hive.io/kvm":  fmt.Sprintf("%v", req.Resources.KVMAvail),
	}

	// Create node state
	node := &types.NodeState{
		ID:            nodeID,
		Tier:          tier,
		Arch:          req.Arch,
		Hostname:      req.Hostname,
		Status:        types.NodeStatusOnline,
		Resources:     req.Resources,
		Hardware:      req.Hardware,
		Labels:        labels,
		JoinedAt:      time.Now().UTC(),
		LastHeartbeat: time.Now().UTC(),
	}

	if req.AgentID != "" {
		node.Agents = []string{req.AgentID}
	}

	// Store node
	if err := r.store.SetNode(node); err != nil {
		r.logger.Error("failed to store node", "error", err)
		r.respondToJoin(msg, types.JoinResponse{
			Accepted: false,
			Error:    "internal error",
		})
		return
	}

	r.logger.Info("node registered",
		"node_id", nodeID,
		"tier", tier,
		"arch", req.Arch,
		"hostname", req.Hostname,
	)

	// Build NATS URL for the joining node
	natsURL := ""
	if r.natsConn != nil {
		// The connected server's URL can be used by the joining node
		natsURL = r.natsConn.ConnectedUrl()
	}

	r.respondToJoin(msg, types.JoinResponse{
		Accepted: true,
		NodeID:   nodeID,
		Tier:     int(tier),
		NATSUrl:  natsURL,
	})
}

// respondToJoin sends a join response back via the NATS reply subject.
func (r *Registry) respondToJoin(msg *nats.Msg, resp types.JoinResponse) {
	if msg.Reply == "" {
		return
	}

	data, err := json.Marshal(resp)
	if err != nil {
		r.logger.Error("failed to marshal join response", "error", err)
		return
	}

	if err := r.natsConn.Publish(msg.Reply, data); err != nil {
		r.logger.Error("failed to publish join response", "error", err)
	}
}

// generateNodeID creates a stable node ID from hostname and arch.
func generateNodeID(hostname, arch string) string {
	if hostname == "" {
		hostname = "unknown"
	}
	return fmt.Sprintf("%s-%s", hostname, arch[:min(8, len(arch))])
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// RecordHeartbeat updates the last heartbeat time for a node.
func (r *Registry) RecordHeartbeat(nodeID string) error {
	node := r.store.GetNode(nodeID)
	if node == nil {
		return fmt.Errorf("node %q not found", nodeID)
	}

	node.LastHeartbeat = time.Now().UTC()
	if node.Status == types.NodeStatusOffline {
		node.Status = types.NodeStatusOnline
	}

	return r.store.SetNode(node)
}

// CheckNodes marks nodes as offline if their heartbeat is stale.
func (r *Registry) CheckNodes(timeout time.Duration) {
	nodes := r.store.AllNodes()
	for _, node := range nodes {
		if node.Status == types.NodeStatusOnline {
			if time.Since(node.LastHeartbeat) > timeout {
				node.Status = types.NodeStatusOffline
				if err := r.store.SetNode(node); err != nil {
					r.logger.Error("failed to mark node offline",
						"node_id", node.ID,
						"error", err,
					)
				} else {
					r.logger.Warn("node marked offline", "node_id", node.ID)
				}
			}
		}
	}
}
