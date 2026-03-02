// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package types

import "time"

// NodeStatus represents the lifecycle status of a node.
type NodeStatus string

const (
	NodeStatusOnline   NodeStatus = "online"
	NodeStatusOffline  NodeStatus = "offline"
	NodeStatusPending  NodeStatus = "pending"
	NodeStatusDraining NodeStatus = "draining"
	NodeStatusCordoned NodeStatus = "cordoned"
)

// NodeTier represents the hardware tier of a node.
type NodeTier int

const (
	NodeTierUnknown NodeTier = 0
	NodeTier1       NodeTier = 1
	NodeTier2       NodeTier = 2
	NodeTier3       NodeTier = 3
)

// NodeState holds the runtime state for a registered node.
type NodeState struct {
	ID            string            `json:"id"`
	Tier          NodeTier          `json:"tier"`
	Arch          string            `json:"arch"`
	Hostname      string            `json:"hostname,omitempty"`
	Status        NodeStatus        `json:"status"`
	Resources     NodeResources     `json:"resources"`
	Hardware      NodeHardware      `json:"hardware,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	Agents        []string          `json:"agents,omitempty"`
	LastHeartbeat time.Time         `json:"last_heartbeat,omitempty"`
	JoinedAt      time.Time         `json:"joined_at"`
}

// NodeResources describes the resources available on a node.
type NodeResources struct {
	MemoryTotal int64 `json:"memory_total"`
	CPUCount    int   `json:"cpu_count"`
	DiskTotal   int64 `json:"disk_total,omitempty"`
	KVMAvail    bool  `json:"kvm_available"`
}

// NodeHardware describes peripheral hardware available on a node.
type NodeHardware struct {
	GPUs        []string `json:"gpus,omitempty"`
	Peripherals []string `json:"peripherals,omitempty"`
}

// JoinRequest is sent by a node when joining the cluster.
type JoinRequest struct {
	Token     string        `json:"token"`
	Hostname  string        `json:"hostname"`
	Arch      string        `json:"arch"`
	Resources NodeResources `json:"resources"`
	Hardware  NodeHardware  `json:"hardware,omitempty"`
	AgentID   string        `json:"agent_id,omitempty"`
}

// JoinResponse is sent by the control plane in response to a join request.
type JoinResponse struct {
	Accepted bool     `json:"accepted"`
	NodeID   string   `json:"node_id,omitempty"`
	Tier     NodeTier `json:"tier,omitempty"`
	Error    string   `json:"error,omitempty"`
	NATSUrl  string   `json:"nats_url,omitempty"`
}

// ClassifyTier determines the node tier based on hardware capabilities.
func ClassifyTier(resources NodeResources) NodeTier {
	// Tier 3: microcontroller-class devices with very low memory (< 512 KB).
	if resources.MemoryTotal > 0 && resources.MemoryTotal < 512*1024 {
		return NodeTier3
	}
	if resources.KVMAvail && resources.MemoryTotal >= 4*1024*1024*1024 {
		return NodeTier1
	}
	return NodeTier2
}
