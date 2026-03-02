// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package node manages node registration, heartbeating, offline detection, and join-token validation.
package node

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
	"golang.org/x/time/rate"
)

// EventPublisher is the interface for publishing lifecycle events.
type EventPublisher interface {
	NodeJoined(nodeID string)
	NodeLeft(nodeID string)
}

// Registry manages node registration and lifecycle.
type Registry struct {
	mu            sync.RWMutex
	store         *state.Store
	natsConn      *nats.Conn
	logger        *slog.Logger
	sub           *nats.Subscription
	hbSub         *nats.Subscription // subscription for node heartbeats
	advertiseAddr string             // externally-reachable NATS URL for joining nodes
	events        EventPublisher     // optional event publisher
	joinLimiter   *rate.Limiter      // overall join request rate limit
	checkStop     chan struct{}      // signals the background CheckNodes goroutine to stop
	checkWg       sync.WaitGroup     // tracks the background CheckNodes goroutine
	started       bool               // prevents double-start
	joinMu        sync.Mutex         // serializes node find+create to prevent TOCTOU races
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
		joinLimiter:   rate.NewLimiter(rate.Limit(10), 5), // 10 joins/s, burst 5
	}
}

// SetEventPublisher sets an optional event publisher for node lifecycle events.
// Safe for concurrent use.
func (r *Registry) SetEventPublisher(ep EventPublisher) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = ep
}

// getEvents returns the current event publisher, safe for concurrent use.
func (r *Registry) getEvents() EventPublisher {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.events
}

// Start subscribes to join requests and node heartbeats on NATS.
func (r *Registry) Start() error {
	r.mu.Lock()
	if r.started {
		r.mu.Unlock()
		return fmt.Errorf("registry already started")
	}
	r.started = true
	r.mu.Unlock()

	sub, err := r.natsConn.Subscribe(protocol.SubjJoinRequest, func(msg *nats.Msg) {
		r.handleJoinRequest(msg)
	})
	if err != nil {
		r.mu.Lock()
		r.started = false
		r.mu.Unlock()
		return fmt.Errorf("subscribing to join requests: %w", err)
	}

	hbSub, err := r.natsConn.Subscribe(protocol.SubjNodeHeartbeat, func(msg *nats.Msg) {
		r.handleNodeHeartbeat(msg)
	})
	if err != nil {
		_ = sub.Unsubscribe()
		r.mu.Lock()
		r.started = false
		r.mu.Unlock()
		return fmt.Errorf("subscribing to node heartbeats: %w", err)
	}

	r.mu.Lock()
	r.sub = sub
	r.hbSub = hbSub
	r.mu.Unlock()

	r.logger.Info("node registry started, listening for join requests and heartbeats")
	return nil
}

// Stop unsubscribes from NATS and stops background goroutines.
func (r *Registry) Stop() error {
	// Stop the background check loop if running.
	r.mu.Lock()
	if r.checkStop != nil {
		close(r.checkStop)
		r.checkStop = nil
	}
	r.mu.Unlock()

	// Wait for the check loop goroutine to finish so we don't race with
	// CheckNodes running after Stop returns (Issue 7 & 37).
	r.checkWg.Wait()

	var firstErr error
	r.mu.Lock()
	sub := r.sub
	hbSub := r.hbSub
	r.sub = nil
	r.hbSub = nil
	r.mu.Unlock()

	// Note: Using Unsubscribe rather than Drain here because these are
	// fire-and-forget handlers (join requests and heartbeats) that do not
	// require in-flight message completion guarantees at shutdown.
	if sub != nil {
		if err := sub.Unsubscribe(); err != nil {
			firstErr = err
		}
	}
	if hbSub != nil {
		if err := hbSub.Unsubscribe(); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	r.mu.Lock()
	r.started = false
	r.mu.Unlock()

	return firstErr
}

// handleJoinRequest processes a join request from a node.
func (r *Registry) handleJoinRequest(msg *nats.Msg) {
	// Rate limit join requests to prevent abuse.
	if r.joinLimiter != nil && !r.joinLimiter.Allow() {
		r.logger.Warn("join request rate limited")
		r.respondToJoin(msg, types.JoinResponse{
			Accepted: false,
			Error:    "rate limit exceeded, try again later",
		})
		return
	}

	// Check reply subject before any side effects (token consumption).
	// Without a reply subject the node cannot receive its assignment, so
	// there is no point in consuming a token use.
	if msg.Reply == "" {
		r.logger.Warn("join request has no reply subject, skipping registration")
		return
	}

	var req types.JoinRequest
	if err := json.Unmarshal(msg.Data, &req); err != nil {
		r.logger.Warn("invalid join request", "error", err)
		r.respondToJoin(msg, types.JoinResponse{
			Accepted: false,
			Error:    "invalid request format",
		})
		return
	}

	// Validate join request fields for safe use in NATS subjects and file paths.
	if req.Hostname == "" {
		r.logger.Warn("join request missing hostname")
		r.respondToJoin(msg, types.JoinResponse{
			Accepted: false,
			Error:    "hostname is required",
		})
		return
	}
	if err := types.ValidateSubjectComponent("hostname", req.Hostname); err != nil {
		r.logger.Warn("join request with invalid hostname", "hostname", req.Hostname, "error", err)
		r.respondToJoin(msg, types.JoinResponse{
			Accepted: false,
			Error:    fmt.Sprintf("invalid hostname: %s", err),
		})
		return
	}
	if req.AgentID != "" {
		if err := types.ValidateSubjectComponent("agent_id", req.AgentID); err != nil {
			r.logger.Warn("join request with invalid agent_id", "agent_id", req.AgentID, "error", err)
			r.respondToJoin(msg, types.JoinResponse{
				Accepted: false,
				Error:    fmt.Sprintf("invalid agent_id: %s", err),
			})
			return
		}
	}
	if req.Arch == "" {
		r.logger.Warn("join request missing arch")
		r.respondToJoin(msg, types.JoinResponse{
			Accepted: false,
			Error:    "arch is required",
		})
		return
	}
	if err := types.ValidateSubjectComponent("arch", req.Arch); err != nil {
		r.logger.Warn("join request with invalid arch", "arch", req.Arch, "error", err)
		r.respondToJoin(msg, types.JoinResponse{
			Accepted: false,
			Error:    fmt.Sprintf("invalid arch: %s", err),
		})
		return
	}

	// Validate resource bounds to reject obviously bogus values.
	const maxResourceMemory = 100 * 1024 * 1024 * 1024 * 1024 // 100 TB
	const maxResourceCPUCount = 10000
	const maxResourceDisk = 100 * 1024 * 1024 * 1024 * 1024 // 100 TB
	if req.Resources.MemoryTotal > maxResourceMemory {
		r.logger.Warn("join request with unreasonable memory value",
			"hostname", req.Hostname,
			"memory_total", req.Resources.MemoryTotal,
		)
		r.respondToJoin(msg, types.JoinResponse{
			Accepted: false,
			Error:    "memory_total exceeds maximum allowed value",
		})
		return
	}
	if req.Resources.CPUCount > maxResourceCPUCount {
		r.logger.Warn("join request with unreasonable CPU count",
			"hostname", req.Hostname,
			"cpu_count", req.Resources.CPUCount,
		)
		r.respondToJoin(msg, types.JoinResponse{
			Accepted: false,
			Error:    "cpu_count exceeds maximum allowed value",
		})
		return
	}
	if req.Resources.DiskTotal > maxResourceDisk {
		r.logger.Warn("join request with unreasonable disk value",
			"hostname", req.Hostname,
			"disk_total", req.Resources.DiskTotal,
		)
		r.respondToJoin(msg, types.JoinResponse{
			Accepted: false,
			Error:    "disk_total exceeds maximum allowed value",
		})
		return
	}

	// Validate hardware arrays to prevent oversized payloads.
	const maxHardwareEntries = 64
	const maxHardwareEntryLen = 256
	if len(req.Hardware.GPUs) > maxHardwareEntries {
		r.respondToJoin(msg, types.JoinResponse{
			Accepted: false,
			Error:    fmt.Sprintf("too many GPUs (max %d)", maxHardwareEntries),
		})
		return
	}
	if len(req.Hardware.Peripherals) > maxHardwareEntries {
		r.respondToJoin(msg, types.JoinResponse{
			Accepted: false,
			Error:    fmt.Sprintf("too many peripherals (max %d)", maxHardwareEntries),
		})
		return
	}
	for _, g := range req.Hardware.GPUs {
		if len(g) > maxHardwareEntryLen {
			r.respondToJoin(msg, types.JoinResponse{
				Accepted: false,
				Error:    fmt.Sprintf("GPU entry exceeds maximum length (%d chars)", maxHardwareEntryLen),
			})
			return
		}
	}
	for _, p := range req.Hardware.Peripherals {
		if len(p) > maxHardwareEntryLen {
			r.respondToJoin(msg, types.JoinResponse{
				Accepted: false,
				Error:    fmt.Sprintf("peripheral entry exceeds maximum length (%d chars)", maxHardwareEntryLen),
			})
			return
		}
	}

	// Reject obviously invalid tokens before consulting the store.
	if len(req.Token) == 0 || len(req.Token) > 1024 {
		r.logger.Warn("join request with invalid token length", "hostname", req.Hostname)
		r.respondToJoin(msg, types.JoinResponse{
			Accepted: false,
			Error:    "invalid or expired token",
		})
		return
	}

	// Warn if TLS is not enabled on the NATS connection.
	if r.natsConn != nil {
		if _, tlsErr := r.natsConn.TLSConnectionState(); tlsErr != nil {
			r.logger.Warn("join tokens are being transmitted without TLS - this is insecure for production",
				"hostname", req.Hostname,
			)
		}
	}

	// Classify tier
	tier := types.ClassifyTier(req.Resources)

	// Hold joinMu across token validation + FindNodeByHostnameArch + SetNode
	// to prevent two concurrent joins from both consuming the same token.
	r.joinMu.Lock()

	// Validate token under joinMu so that token consumption and node
	// registration are atomic. Without this, two concurrent joins could
	// both validate and consume the same MaxUses=1 token.
	// SECURITY: Join tokens traverse NATS in plaintext. TLS MUST be enabled for production deployments.
	// NOTE: ValidateToken atomically increments the token's usage count. If the
	// registration fails after this point (e.g., store error), we attempt a
	// best-effort compensating decrement via DecrementTokenUsage to avoid
	// permanently consuming the token use for a failed operation.
	token := r.store.ValidateToken(req.Token)
	if token == nil {
		r.joinMu.Unlock()
		r.logger.Warn("join request with invalid token", "hostname", req.Hostname)
		r.respondToJoin(msg, types.JoinResponse{
			Accepted: false,
			Error:    "invalid or expired token",
		})
		return
	}
	// Zero the token from the request to prevent it from lingering in memory.
	req.Token = ""

	// Check for existing node with same hostname+arch combo (prevent duplicates).
	// If found, update the existing node instead of creating a new one.
	const maxNodes = 100000
	var nodeID string
	var existingNode *types.NodeState
	if existingNode = r.store.FindNodeByHostnameArch(req.Hostname, req.Arch); existingNode != nil {
		nodeID = existingNode.ID
		r.logger.Info("existing node rejoining",
			"node_id", nodeID,
			"hostname", req.Hostname,
			"arch", req.Arch,
		)
	} else {
		// Enforce maximum node count for new registrations (not rejoins).
		if r.store.NodeCount() >= maxNodes {
			r.joinMu.Unlock()
			r.logger.Warn("maximum node count reached, rejecting join",
				"hostname", req.Hostname,
				"max_nodes", maxNodes,
			)
			r.respondToJoin(msg, types.JoinResponse{
				Accepted: false,
				Error:    fmt.Sprintf("maximum number of nodes (%d) reached", maxNodes),
			})
			return
		}
		var err error
		nodeID, err = generateNodeID(req.Hostname, req.Arch)
		if err != nil {
			r.joinMu.Unlock()
			r.logger.Error("failed to generate node ID", "error", err)
			r.respondToJoin(msg, types.JoinResponse{
				Accepted: false,
				Error:    "internal error",
			})
			return
		}
	}

	// Build labels
	labels := map[string]string{
		"hive.io/arch": req.Arch,
		"hive.io/tier": fmt.Sprintf("%d", tier),
		"hive.io/kvm":  fmt.Sprintf("%v", req.Resources.KVMAvail),
	}

	// Create node state
	now := time.Now().UTC()
	node := &types.NodeState{
		ID:            nodeID,
		Tier:          tier,
		Arch:          req.Arch,
		Hostname:      req.Hostname,
		Status:        types.NodeStatusOnline,
		Resources:     req.Resources,
		Hardware:      req.Hardware,
		Labels:        labels,
		JoinedAt:      now,
		LastHeartbeat: now,
	}

	// Preserve the original join time for rejoining nodes.
	if existingNode != nil {
		node.JoinedAt = existingNode.JoinedAt
	}

	if req.AgentID != "" {
		if r.store.GetAgent(req.AgentID) == nil {
			r.logger.Warn("join request references unknown agent_id, agent may be created later",
				"agent_id", req.AgentID,
				"hostname", req.Hostname,
			)
		}
		node.Agents = []string{req.AgentID}
	}

	// Store node
	if err := r.store.SetNode(node); err != nil {
		r.joinMu.Unlock()
		r.logger.Error("failed to store node", "error", err)

		// Best-effort: decrement the token usage since the join failed.
		// This prevents a permanently consumed token use for a failed registration.
		if decErr := r.store.DecrementTokenUsage(token.Hash); decErr != nil {
			r.logger.Warn("failed to decrement token usage after failed node registration",
				"token_prefix", token.Prefix,
				"error", decErr,
			)
		}

		r.respondToJoin(msg, types.JoinResponse{
			Accepted: false,
			Error:    "internal error",
		})
		return
	}

	r.joinMu.Unlock()

	if ep := r.getEvents(); ep != nil {
		ep.NodeJoined(nodeID)
	}

	r.logger.Info("node registered",
		"node_id", nodeID,
		"tier", tier,
		"arch", req.Arch,
		"hostname", req.Hostname,
		"rejoined", existingNode != nil,
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
		Tier:     tier,
		NATSUrl:  natsURL,
	})
}

// respondToJoin sends a join response back via the NATS reply subject.
// Returns true if the response was sent, false if the reply subject was missing.
func (r *Registry) respondToJoin(msg *nats.Msg, resp types.JoinResponse) bool {
	if msg.Reply == "" {
		r.logger.Warn("join request has no reply subject, cannot send response",
			"accepted", resp.Accepted)
		return false
	}

	data, err := json.Marshal(resp)
	if err != nil {
		r.logger.Error("failed to marshal join response", "error", err)
		return false
	}

	if err := r.natsConn.Publish(msg.Reply, data); err != nil {
		r.logger.Error("failed to publish join response", "error", err)
		return false
	}
	return true
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

	if err := env.Validate(); err != nil {
		r.logger.Warn("node heartbeat envelope validation failed", "error", err)
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

	// Cross-validate: the node ID in the envelope must match the node ID
	// encoded in the NATS subject (hive.node.<nodeID>.heartbeat). This prevents
	// a compromised node from spoofing heartbeats for a different node.
	// Reject messages where the subject doesn't have exactly 4 parts.
	parts := strings.Split(msg.Subject, ".")
	if len(parts) != 4 {
		r.logger.Warn("node heartbeat has unexpected subject format", "subject", msg.Subject)
		return
	}
	subjectNodeID := parts[2]
	if subjectNodeID != nodeID {
		r.logger.Warn("node heartbeat subject/from mismatch, dropping",
			"subject_node_id", subjectNodeID,
			"envelope_from", nodeID,
		)
		return
	}

	if err := r.RecordHeartbeat(nodeID); err != nil {
		r.logger.Debug("node heartbeat for unknown node", "node_id", nodeID, "error", err)
		return
	}

	r.logger.Debug("node heartbeat received", "node_id", nodeID)
}

// generateNodeID creates a unique node ID from hostname and arch with a
// random 12-hex-character (48-bit) suffix to prevent collisions among nodes
// that share the same default hostname (e.g. multiple Raspberry Pis with
// "raspberrypi"). The previous 24-bit suffix had non-trivial collision
// probability at scale; 48 bits provides ~2^24 nodes before a 50% collision
// chance (birthday bound).
func generateNodeID(hostname, arch string) (string, error) {
	if hostname == "" {
		hostname = "unknown"
	}
	archPart := arch[:min(8, len(arch))]
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("crypto/rand failed: %w", err)
	}
	return fmt.Sprintf("%s-%s-%s", hostname, archPart, hex.EncodeToString(buf[:])), nil
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

// getOutboundIP returns the first non-loopback IPv4 address found by
// enumerating local network interfaces. This avoids making an outbound UDP
// connection to a public address (which may fail in air-gapped environments).
// Returns "" if no suitable address is found.
func getOutboundIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && ipnet.IP.To4() != nil {
				return ipnet.IP.String()
			}
		}
	}
	return ""
}

// RecordHeartbeat updates the last heartbeat time for a node using an atomic
// read-modify-write to prevent race conditions.
func (r *Registry) RecordHeartbeat(nodeID string) error {
	return r.store.ModifyNode(nodeID, func(node *types.NodeState) error {
		node.LastHeartbeat = time.Now().UTC()
		if node.Status == types.NodeStatusOffline {
			node.Status = types.NodeStatusOnline
		}
		return nil
	})
}

// CheckNodes marks nodes as offline if their heartbeat is stale.
// It uses ModifyNode for atomic read-modify-write to avoid TOCTOU races
// with concurrent heartbeat updates.
func (r *Registry) CheckNodes(timeout time.Duration) {
	nodes := r.store.AllNodes()
	for _, node := range nodes {
		if node.Status != types.NodeStatusOnline {
			continue
		}

		nodeID := node.ID
		wentOffline := false

		err := r.store.ModifyNode(nodeID, func(n *types.NodeState) error {
			// Re-check inside the atomic callback: the node may have received
			// a heartbeat between AllNodes() and ModifyNode acquiring the lock.
			if n.Status != types.NodeStatusOnline {
				return nil // node already transitioned, nothing to do
			}
			if time.Since(n.LastHeartbeat) > timeout {
				n.Status = types.NodeStatusOffline
				wentOffline = true
			}
			return nil
		})

		if err != nil {
			r.logger.Error("failed to mark node offline",
				"node_id", nodeID,
				"error", err,
			)
			continue
		}

		if wentOffline {
			if ep := r.getEvents(); ep != nil {
				ep.NodeLeft(nodeID)
			}
			r.logger.Warn("node marked offline", "node_id", nodeID)
		}
	}
}

// StartCheckLoop starts a background goroutine that calls CheckNodes at the
// given interval with the given timeout. The loop stops when Stop is called.
// It is safe to call multiple times, but a second call while a loop is already
// running is a no-op and logs a warning.
func (r *Registry) StartCheckLoop(interval, timeout time.Duration) {
	r.mu.Lock()
	if r.checkStop != nil {
		// A check loop is already running; avoid leaking a second goroutine.
		r.mu.Unlock()
		r.logger.Warn("StartCheckLoop called while check loop is already running, ignoring")
		return
	}
	r.checkStop = make(chan struct{})
	stopCh := r.checkStop
	r.mu.Unlock()

	r.checkWg.Add(1)
	go func() {
		defer r.checkWg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				r.CheckNodes(timeout)
			}
		}
	}()
}
