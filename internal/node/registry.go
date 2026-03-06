package node

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// Registry manages node registration and lifecycle.
type Registry struct {
	store         *state.Store
	natsConn      *nats.Conn
	logger        *slog.Logger
	sub           *nats.Subscription
	hbSub         *nats.Subscription // subscription for node heartbeats
	advertiseAddr string             // externally-reachable NATS URL for joining nodes
}

// NewRegistry creates a new node registry.
// advertiseAddr is the externally-reachable NATS URL (e.g. "nats://192.168.1.10:4222")
// that joining nodes should use. If empty, the registry falls back to the
// connected URL, which may be a loopback address.
func NewRegistry(store *state.Store, nc *nats.Conn, logger *slog.Logger, advertiseAddr string) *Registry {
	return &Registry{
		store:         store,
		natsConn:      nc,
		logger:        logger.With("component", "node-registry"),
		advertiseAddr: advertiseAddr,
	}
}

// Start subscribes to join requests and node heartbeats on NATS.
func (r *Registry) Start() error {
	sub, err := r.natsConn.Subscribe("hive.join.request", func(msg *nats.Msg) {
		r.handleJoinRequest(msg)
	})
	if err != nil {
		return fmt.Errorf("subscribing to join requests: %w", err)
	}
	r.sub = sub

	hbSub, err := r.natsConn.Subscribe("hive.node.*.heartbeat", func(msg *nats.Msg) {
		r.handleNodeHeartbeat(msg)
	})
	if err != nil {
		return fmt.Errorf("subscribing to node heartbeats: %w", err)
	}
	r.hbSub = hbSub

	r.logger.Info("node registry started, listening for join requests and heartbeats")
	return nil
}

// Stop unsubscribes from NATS.
func (r *Registry) Stop() error {
	var firstErr error
	if r.sub != nil {
		if err := r.sub.Unsubscribe(); err != nil {
			firstErr = err
		}
	}
	if r.hbSub != nil {
		if err := r.hbSub.Unsubscribe(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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

	// Build NATS URL for the joining node. Prefer the explicitly configured
	// advertise address so that the URL is reachable from remote nodes.
	// Fall back to the local connection URL only if no advertise address is set.
	natsURL := r.advertiseAddr
	if natsURL == "" && r.natsConn != nil {
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

// handleNodeHeartbeat processes a heartbeat message from a registered node.
// Nodes publish heartbeats on hive.node.{nodeID}.heartbeat using the standard
// Envelope format with Type = "node-heartbeat" and From = nodeID.
func (r *Registry) handleNodeHeartbeat(msg *nats.Msg) {
	var env types.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		r.logger.Warn("invalid node heartbeat envelope", "error", err)
		return
	}

	if env.Type != types.MessageTypeNodeHeartbeat {
		return
	}

	nodeID := env.From
	if nodeID == "" {
		r.logger.Warn("node heartbeat missing node ID")
		return
	}

	if err := r.RecordHeartbeat(nodeID); err != nil {
		r.logger.Debug("node heartbeat for unknown node", "node_id", nodeID, "error", err)
		return
	}

	r.logger.Debug("node heartbeat received", "node_id", nodeID)
}

// generateNodeID creates a unique node ID from hostname and arch with a
// random 6-hex-character suffix to prevent collisions among nodes that share
// the same default hostname (e.g. multiple Raspberry Pis with "raspberrypi").
func generateNodeID(hostname, arch string) string {
	if hostname == "" {
		hostname = "unknown"
	}
	archPart := arch[:min(8, len(arch))]
	var buf [3]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// Fallback: use a timestamp-derived suffix if crypto/rand fails.
		return fmt.Sprintf("%s-%s-%06x", hostname, archPart, time.Now().UnixNano()&0xffffff)
	}
	return fmt.Sprintf("%s-%s-%s", hostname, archPart, hex.EncodeToString(buf[:]))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ResolveAdvertiseAddr builds a NATS URL suitable for advertising to remote
// nodes based on the cluster's NATS configuration. If host is a specific
// non-loopback address, it is used directly. If host is "0.0.0.0" or empty,
// the function discovers the machine's first non-loopback IPv4 address.
// Returns "" if no suitable address can be determined.
func ResolveAdvertiseAddr(host string, port int) string {
	if port == 0 {
		port = 4222
	}

	// If the configured host is a specific, non-wildcard, non-loopback address, use it.
	if host != "" && host != "0.0.0.0" && host != "::" {
		ip := net.ParseIP(host)
		if ip != nil && !ip.IsLoopback() {
			return fmt.Sprintf("nats://%s:%d", host, port)
		}
		// host might be a hostname; if it's not "localhost", use it as-is.
		if host != "localhost" && host != "127.0.0.1" && host != "::1" {
			return fmt.Sprintf("nats://%s:%d", host, port)
		}
	}

	// Host is empty, 0.0.0.0, ::, or loopback — try to find a non-loopback interface IP.
	if outIP := getOutboundIP(); outIP != "" {
		return fmt.Sprintf("nats://%s:%d", outIP, port)
	}

	// Could not determine an external address.
	return ""
}

// getOutboundIP returns the preferred outbound IPv4 address of this machine
// by dialing a UDP socket to a public address (no actual traffic is sent).
// Returns "" if no suitable address is found.
func getOutboundIP() string {
	conn, err := net.Dial("udp4", "8.8.8.8:80")
	if err != nil {
		return ""
	}
	defer conn.Close()

	localAddr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return ""
	}
	return localAddr.IP.String()
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
