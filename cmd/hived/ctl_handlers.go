// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"time"

	"github.com/brmurrell3/hive/internal/auth"
	"github.com/brmurrell3/hive/internal/config"
	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/token"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
)

// validLabelKey matches Kubernetes-style label keys: alphanumeric with dots,
// dashes, underscores, and slashes (for prefixed keys like "app.kubernetes.io/name").
// Maximum length is enforced separately (253 chars).
var validLabelKey = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/-]*$`)

// validLabelValue matches label values: alphanumeric with dots, dashes, and
// underscores. An empty string is a valid label value (used for key-only labels).
// Maximum length is enforced separately (63 chars).
var validLabelValue = regexp.MustCompile(`^[a-zA-Z0-9._-]*$`)

const (
	maxLabelKeyLen   = 253
	maxLabelValueLen = 63
	maxLabelsPerNode = 256
)

// validateLabelKey checks that a label key conforms to Kubernetes-style rules:
// alphanumeric with dots, dashes, underscores, slashes, max 253 characters.
func validateLabelKey(key string) error {
	if key == "" {
		return fmt.Errorf("label key must not be empty")
	}
	if len(key) > maxLabelKeyLen {
		return fmt.Errorf("label key exceeds maximum length of %d characters", maxLabelKeyLen)
	}
	if !validLabelKey.MatchString(key) {
		return fmt.Errorf("label key contains invalid characters (allowed: alphanumeric, dots, dashes, underscores, slashes)")
	}
	return nil
}

// validateLabelValue checks that a label value is max 63 characters and
// contains only safe characters.
func validateLabelValue(value string) error {
	if len(value) > maxLabelValueLen {
		return fmt.Errorf("label value exceeds maximum length of %d characters", maxLabelValueLen)
	}
	if value != "" && !validLabelValue.MatchString(value) {
		return fmt.Errorf("label value contains invalid characters (allowed: alphanumeric, dots, dashes, underscores)")
	}
	return nil
}

// --- Node handlers ---

// nodeActionRequest is the payload for node operations that need extra fields.
type nodeActionRequest struct {
	NodeID string            `json:"node_id"`
	Labels map[string]string `json:"labels,omitempty"`
	Keys   []string          `json:"keys,omitempty"`
}

func (h *controlHandler) handleNodesList(msg *nats.Msg) {
	h.withAuth(msg, "view", "nodes", func(req *protocol.CtlRequest, env *types.Envelope) {
		nodes := h.store.AllNodes()
		h.respondData(msg, nodes)
	})
}

func (h *controlHandler) handleNodesStatus(msg *nats.Msg) {
	h.withAuth(msg, "view", "nodes", func(req *protocol.CtlRequest, env *types.Envelope) {
		if !h.validateID(msg, "node_id", req.AgentID) {
			return
		}
		n := h.store.GetNode(req.AgentID)
		if n == nil {
			h.respondError(msg, fmt.Sprintf("node %q not found", req.AgentID))
			return
		}
		h.respondData(msg, n)
	})
}

func (h *controlHandler) handleNodesDrain(msg *nats.Msg) {
	h.withAuth(msg, "manage", "nodes", func(req *protocol.CtlRequest, env *types.Envelope) {
		if !h.validateID(msg, "node_id", req.AgentID) {
			return
		}
		nodeID := req.AgentID
		if err := h.store.ModifyNode(nodeID, func(n *types.NodeState) error {
			if n.Status != types.NodeStatusDraining {
				n.Status = types.NodeStatusDraining
			}
			return nil
		}); err != nil {
			h.respondError(msg, fmt.Sprintf("updating node state: %v", err))
			return
		}
		h.respond(msg, &protocol.CtlResponse{Success: true})
	})
}

func (h *controlHandler) handleNodesCordon(msg *nats.Msg) {
	h.withAuth(msg, "manage", "nodes", func(req *protocol.CtlRequest, env *types.Envelope) {
		if !h.validateID(msg, "node_id", req.AgentID) {
			return
		}
		nodeID := req.AgentID
		if err := h.store.ModifyNode(nodeID, func(n *types.NodeState) error {
			if n.Status != types.NodeStatusCordoned {
				n.Status = types.NodeStatusCordoned
			}
			return nil
		}); err != nil {
			h.respondError(msg, fmt.Sprintf("updating node state: %v", err))
			return
		}
		h.respond(msg, &protocol.CtlResponse{Success: true})
	})
}

func (h *controlHandler) handleNodesUncordon(msg *nats.Msg) {
	h.withAuth(msg, "manage", "nodes", func(req *protocol.CtlRequest, env *types.Envelope) {
		if !h.validateID(msg, "node_id", req.AgentID) {
			return
		}
		nodeID := req.AgentID
		if err := h.store.ModifyNode(nodeID, func(n *types.NodeState) error {
			if n.Status == types.NodeStatusCordoned || n.Status == types.NodeStatusDraining {
				n.Status = types.NodeStatusOnline
			}
			return nil
		}); err != nil {
			h.respondError(msg, fmt.Sprintf("updating node state: %v", err))
			return
		}
		h.respond(msg, &protocol.CtlResponse{Success: true})
	})
}

func (h *controlHandler) handleNodesLabel(msg *nats.Msg) {
	h.withAuth(msg, "manage", "nodes", func(req *protocol.CtlRequest, env *types.Envelope) {
		var labelReq nodeActionRequest
		if err := json.Unmarshal(env.Payload, &labelReq); err != nil {
			h.logger.Warn("invalid label request", "error", err)
			h.respondError(msg, "invalid label request")
			return
		}
		if !h.validateID(msg, "node_id", labelReq.NodeID) {
			return
		}
		if len(labelReq.Labels) == 0 {
			h.respondError(msg, "labels must not be empty")
			return
		}
		for k, v := range labelReq.Labels {
			if err := validateLabelKey(k); err != nil {
				h.respondError(msg, fmt.Sprintf("invalid label key %q: %v", k, err))
				return
			}
			if err := validateLabelValue(v); err != nil {
				h.respondError(msg, fmt.Sprintf("invalid label value for key %q: %v", k, err))
				return
			}
		}
		if err := h.store.ModifyNode(labelReq.NodeID, func(n *types.NodeState) error {
			if n.Labels == nil {
				n.Labels = make(map[string]string)
			}
			// Count how many new labels would be added (not already present).
			newCount := 0
			for k := range labelReq.Labels {
				if _, exists := n.Labels[k]; !exists {
					newCount++
				}
			}
			if len(n.Labels)+newCount > maxLabelsPerNode {
				return fmt.Errorf("node would exceed maximum of %d labels", maxLabelsPerNode)
			}
			for k, v := range labelReq.Labels {
				n.Labels[k] = v
			}
			return nil
		}); err != nil {
			h.respondError(msg, fmt.Sprintf("updating node state: %v", err))
			return
		}
		h.respond(msg, &protocol.CtlResponse{Success: true})
	})
}

func (h *controlHandler) handleNodesUnlabel(msg *nats.Msg) {
	h.withAuth(msg, "manage", "nodes", func(req *protocol.CtlRequest, env *types.Envelope) {
		var unlabelReq nodeActionRequest
		if err := json.Unmarshal(env.Payload, &unlabelReq); err != nil {
			h.logger.Warn("invalid unlabel request", "error", err)
			h.respondError(msg, "invalid unlabel request")
			return
		}
		if !h.validateID(msg, "node_id", unlabelReq.NodeID) {
			return
		}
		if len(unlabelReq.Keys) == 0 {
			h.respondError(msg, "keys must not be empty")
			return
		}
		if len(unlabelReq.Keys) > maxLabelsPerNode {
			h.respondError(msg, fmt.Sprintf("too many keys in unlabel request (max %d)", maxLabelsPerNode))
			return
		}
		for _, key := range unlabelReq.Keys {
			if err := validateLabelKey(key); err != nil {
				h.respondError(msg, fmt.Sprintf("invalid label key %q: %v", key, err))
				return
			}
		}
		if err := h.store.ModifyNode(unlabelReq.NodeID, func(n *types.NodeState) error {
			for _, key := range unlabelReq.Keys {
				delete(n.Labels, key)
			}
			return nil
		}); err != nil {
			h.respondError(msg, fmt.Sprintf("updating node state: %v", err))
			return
		}
		h.respond(msg, &protocol.CtlResponse{Success: true})
	})
}

func (h *controlHandler) handleNodesApprove(msg *nats.Msg) {
	h.withAuth(msg, "manage", "nodes", func(req *protocol.CtlRequest, env *types.Envelope) {
		if !h.validateID(msg, "node_id", req.AgentID) {
			return
		}
		nodeID := req.AgentID
		if err := h.store.ModifyNode(nodeID, func(n *types.NodeState) error {
			if n.Status != types.NodeStatusPending {
				return fmt.Errorf("node %q is in %q status, only nodes in %q status can be approved", nodeID, n.Status, types.NodeStatusPending)
			}
			n.Status = types.NodeStatusOnline
			return nil
		}); err != nil {
			h.respondError(msg, fmt.Sprintf("updating node state: %v", err))
			return
		}
		h.respond(msg, &protocol.CtlResponse{Success: true})
	})
}

func (h *controlHandler) handleNodesRemove(msg *nats.Msg) {
	h.withAuth(msg, "manage", "nodes", func(req *protocol.CtlRequest, env *types.Envelope) {
		if !h.validateID(msg, "node_id", req.AgentID) {
			return
		}
		nodeID := req.AgentID
		n := h.store.GetNode(nodeID)
		if n == nil {
			h.respondError(msg, fmt.Sprintf("node %q not found", nodeID))
			return
		}
		if err := h.store.RemoveNode(nodeID); err != nil {
			h.respondError(msg, fmt.Sprintf("removing node: %v", err))
			return
		}
		// Clean up stale node metric labels to prevent unbounded cardinality growth.
		h.metrics.RemoveNode(nodeID)
		h.respond(msg, &protocol.CtlResponse{Success: true})
	})
}

// --- Token handlers ---

type tokenCreateRequest struct {
	TTL string `json:"ttl,omitempty"`
}

func (h *controlHandler) handleTokensCreate(msg *nats.Msg) {
	h.withAuth(msg, "manage", "tokens", func(req *protocol.CtlRequest, env *types.Envelope) {
		var createReq tokenCreateRequest
		if err := json.Unmarshal(env.Payload, &createReq); err != nil {
			h.logger.Warn("invalid token create request", "error", err)
			h.respondError(msg, "invalid token create request")
			return
		}
		var ttlDuration time.Duration
		if createReq.TTL != "" {
			var err error
			ttlDuration, err = time.ParseDuration(createReq.TTL)
			if err != nil {
				h.respondError(msg, fmt.Sprintf("invalid TTL %q: %v", createReq.TTL, err))
				return
			}
		}
		rawToken, err := token.Generate(h.store, ttlDuration, 0)
		if err != nil {
			h.respondError(msg, fmt.Sprintf("generating token: %v", err))
			return
		}
		h.respondData(msg, map[string]string{"token": rawToken})
	})
}

func (h *controlHandler) handleTokensList(msg *nats.Msg) {
	h.withAuth(msg, "view", "tokens", func(req *protocol.CtlRequest, env *types.Envelope) {
		tokens := h.store.AllTokens()
		h.respondData(msg, tokens)
	})
}

func (h *controlHandler) handleTokensRevoke(msg *nats.Msg) {
	h.withAuth(msg, "manage", "tokens", func(req *protocol.CtlRequest, env *types.Envelope) {
		var revokeReq struct {
			Prefix string `json:"prefix"`
		}
		if err := json.Unmarshal(env.Payload, &revokeReq); err != nil {
			h.logger.Warn("invalid revoke request", "error", err)
			h.respondError(msg, "invalid revoke request")
			return
		}
		if revokeReq.Prefix == "" {
			h.respondError(msg, "token prefix must not be empty")
			return
		}
		if err := h.store.RevokeToken(revokeReq.Prefix); err != nil {
			h.respondError(msg, fmt.Sprintf("revoking token: %v", err))
			return
		}
		h.respond(msg, &protocol.CtlResponse{Success: true})
	})
}

// --- User handlers ---

type userCreateRequest struct {
	UserID string   `json:"user_id"`
	Role   string   `json:"role"`
	Teams  []string `json:"teams,omitempty"`
	Agents []string `json:"agents,omitempty"`
}

type userUpdateRequest struct {
	UserID      string   `json:"user_id"`
	Role        string   `json:"role,omitempty"`
	Teams       []string `json:"teams,omitempty"`
	Agents      []string `json:"agents,omitempty"`
	ClearTeams  bool     `json:"clear_teams,omitempty"`
	ClearAgents bool     `json:"clear_agents,omitempty"`
}

func (h *controlHandler) handleUsersCreate(msg *nats.Msg) {
	h.withAuth(msg, "manage", "users", func(req *protocol.CtlRequest, env *types.Envelope) {
		var createReq userCreateRequest
		if err := json.Unmarshal(env.Payload, &createReq); err != nil {
			h.logger.Warn("invalid user create request", "error", err)
			h.respondError(msg, "invalid user create request")
			return
		}
		if !h.validateID(msg, "user_id", createReq.UserID) {
			return
		}
		if err := auth.ValidateRole(auth.Role(createReq.Role)); err != nil {
			h.respondError(msg, fmt.Sprintf("invalid role: %v", err))
			return
		}
		rawToken, err := generateUserTokenHived()
		if err != nil {
			h.respondError(msg, fmt.Sprintf("generating token: %v", err))
			return
		}
		tokenHash := auth.HashToken(rawToken)
		user := &auth.User{
			ID:        createReq.UserID,
			Role:      auth.Role(createReq.Role),
			TokenHash: tokenHash,
			Teams:     createReq.Teams,
			Agents:    createReq.Agents,
		}
		if err := h.store.AddUser(user); err != nil {
			h.respondError(msg, fmt.Sprintf("adding user: %v", err))
			return
		}
		h.rebuildAuth()
		h.respondData(msg, map[string]string{
			"user_id": createReq.UserID,
			"role":    createReq.Role,
			"token":   rawToken,
		})
	})
}

func (h *controlHandler) handleUsersList(msg *nats.Msg) {
	h.withAuth(msg, "view", "users", func(req *protocol.CtlRequest, env *types.Envelope) {
		users := h.store.AllUsers()
		h.respondData(msg, users)
	})
}

func (h *controlHandler) handleUsersUpdate(msg *nats.Msg) {
	h.withAuth(msg, "manage", "users", func(req *protocol.CtlRequest, env *types.Envelope) {
		var updateReq userUpdateRequest
		if err := json.Unmarshal(env.Payload, &updateReq); err != nil {
			h.logger.Warn("invalid user update request", "error", err)
			h.respondError(msg, "invalid user update request")
			return
		}
		if !h.validateID(msg, "user_id", updateReq.UserID) {
			return
		}
		// Validate role before entering atomic section to fail fast.
		if updateReq.Role != "" {
			if err := auth.ValidateRole(auth.Role(updateReq.Role)); err != nil {
				h.respondError(msg, fmt.Sprintf("invalid role: %v", err))
				return
			}
		}
		// Use ModifyUser for atomic read-modify-write to avoid TOCTOU race
		// between GetUser and UpdateUser.
		if err := h.store.ModifyUser(updateReq.UserID, func(user *auth.User) error {
			if updateReq.Role != "" {
				user.Role = auth.Role(updateReq.Role)
			}
			if updateReq.ClearTeams {
				user.Teams = nil
			} else if len(updateReq.Teams) > 0 {
				user.Teams = updateReq.Teams
			}
			if updateReq.ClearAgents {
				user.Agents = nil
			} else if len(updateReq.Agents) > 0 {
				user.Agents = updateReq.Agents
			}
			return nil
		}); err != nil {
			h.respondError(msg, fmt.Sprintf("updating user: %v", err))
			return
		}
		h.rebuildAuth()
		h.respond(msg, &protocol.CtlResponse{Success: true})
	})
}

func (h *controlHandler) handleUsersRevoke(msg *nats.Msg) {
	h.withAuth(msg, "manage", "users", func(req *protocol.CtlRequest, env *types.Envelope) {
		if !h.validateID(msg, "user_id", req.AgentID) {
			return
		}
		if err := h.store.RemoveUser(req.AgentID); err != nil {
			h.respondError(msg, fmt.Sprintf("revoking user: %v", err))
			return
		}
		h.rebuildAuth()
		h.respond(msg, &protocol.CtlResponse{Success: true})
	})
}

func (h *controlHandler) handleUsersRotate(msg *nats.Msg) {
	h.withAuth(msg, "manage", "users", func(req *protocol.CtlRequest, env *types.Envelope) {
		if !h.validateID(msg, "user_id", req.AgentID) {
			return
		}
		rawToken, err := generateUserTokenHived()
		if err != nil {
			h.respondError(msg, fmt.Sprintf("generating token: %v", err))
			return
		}
		if err := h.store.ModifyUser(req.AgentID, func(user *auth.User) error {
			user.TokenHash = auth.HashToken(rawToken)
			return nil
		}); err != nil {
			h.respondError(msg, fmt.Sprintf("updating user token: %v", err))
			return
		}
		h.rebuildAuth()
		h.respondData(msg, map[string]string{"token": rawToken})
	})
}

func generateUserTokenHived() (string, error) {
	b := make([]byte, 32)
	if _, err := crand.Read(b); err != nil {
		return "", fmt.Errorf("generating random bytes: %w", err)
	}
	return "hive-user-" + hex.EncodeToString(b), nil
}

// --- Status handler ---

type clusterStatusResponse struct {
	ClusterName  string         `json:"cluster_name"`
	NodeCount    int            `json:"node_count"`
	TeamCount    int            `json:"team_count"`
	AgentCount   int            `json:"agent_count"`
	StatusCounts map[string]int `json:"status_counts,omitempty"`
	NATSPort     int            `json:"nats_port"`
}

func (h *controlHandler) handleClusterStatus(msg *nats.Msg) {
	h.withAuth(msg, "view", "status", func(req *protocol.CtlRequest, env *types.Envelope) {
		cfg, err := config.LoadCluster(h.clusterRoot)
		if err != nil {
			h.logger.Error("loading cluster config", "error", err)
			h.respondError(msg, "internal error loading cluster config")
			return
		}
		agents := h.store.AllAgents()
		nodes := h.store.AllNodes()
		teams, err := config.LoadTeams(h.clusterRoot)
		if err != nil {
			h.logger.Error("loading teams", "error", err)
			h.respondError(msg, "internal error loading teams")
			return
		}
		statusCounts := make(map[string]int)
		for _, a := range agents {
			statusCounts[string(a.Status)]++
		}
		resp := clusterStatusResponse{
			ClusterName:  cfg.Metadata.Name,
			NodeCount:    len(nodes),
			TeamCount:    len(teams),
			AgentCount:   len(agents),
			StatusCounts: statusCounts,
			NATSPort:     cfg.Spec.NATS.Port,
		}
		h.respondData(msg, resp)
	})
}
