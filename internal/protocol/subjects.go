// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package protocol defines shared NATS subject constants and wire-format
// types used by both hived (server) and hivectl (client).
package protocol

// Control-plane NATS subjects for agent management.
const (
	SubjAgentStart   = "hive.ctl.agents.start"
	SubjAgentStop    = "hive.ctl.agents.stop"
	SubjAgentRestart = "hive.ctl.agents.restart"
	SubjAgentDestroy = "hive.ctl.agents.destroy"
	SubjAgentStatus  = "hive.ctl.agents.status"
	SubjAgentList    = "hive.ctl.agents.list"
)

// Control-plane NATS subjects for node management.
const (
	SubjNodeList     = "hive.ctl.nodes.list"
	SubjNodeStatus   = "hive.ctl.nodes.status"
	SubjNodeDrain    = "hive.ctl.nodes.drain"
	SubjNodeCordon   = "hive.ctl.nodes.cordon"
	SubjNodeUncordon = "hive.ctl.nodes.uncordon"
	SubjNodeLabel    = "hive.ctl.nodes.label"
	SubjNodeUnlabel  = "hive.ctl.nodes.unlabel"
	SubjNodeApprove  = "hive.ctl.nodes.approve"
	SubjNodeRemove   = "hive.ctl.nodes.remove"
)

// Control-plane NATS subjects for token management.
const (
	SubjTokenCreate = "hive.ctl.tokens.create" //nolint:gosec // NATS subject, not a credential
	SubjTokenList   = "hive.ctl.tokens.list"   //nolint:gosec // NATS subject, not a credential
	SubjTokenRevoke = "hive.ctl.tokens.revoke" //nolint:gosec // NATS subject, not a credential
)

// Control-plane NATS subjects for user management.
const (
	SubjUserCreate = "hive.ctl.users.create"
	SubjUserList   = "hive.ctl.users.list"
	SubjUserUpdate = "hive.ctl.users.update"
	SubjUserRevoke = "hive.ctl.users.revoke"
	SubjUserRotate = "hive.ctl.users.rotate"
)

// Control-plane NATS subjects for capability management.
const (
	SubjCapabilityList      = "hive.ctl.capabilities.list"
	SubjCapabilityDescribe  = "hive.ctl.capabilities.describe"
	SubjCapabilityProviders = "hive.ctl.capabilities.providers"
)

// SubjStatus is the control-plane NATS subject for cluster status.
const SubjStatus = "hive.ctl.status"

// Dynamic NATS subject format strings. Use with fmt.Sprintf to inject
// agent IDs, capability names, or team IDs.
const (
	FmtAgentMemory      = "hive.agent.%s.memory"            // agent memory updates
	FmtAgentInbox       = "hive.agent.%s.inbox"             // agent chat inbox
	FmtAgentSidecarExec = "hive.agent.%s.sidecar.exec"      // remote exec via sidecar
	FmtAgentControl     = "hive.control.%s"                 // agent control messages
	FmtAgentHealth      = "hive.health.%s"                  // agent health heartbeats
	FmtCapabilityReq    = "hive.capabilities.%s.%s.request" // capability invocation
	FmtTeamBroadcast    = "hive.team.%s.broadcast"          // team-wide broadcast
	FmtTeamResult       = "hive.team.%s.result"             // pipeline result
)

// Static NATS subjects used by individual subsystems.
const (
	SubjJoinRequest        = "hive.join.request"          // node join requests
	SubjNodeHeartbeat      = "hive.node.*.heartbeat"      // node heartbeat (wildcard)
	SubjHealthAll          = "hive.health.*"              // health heartbeats (wildcard)
	SubjLogsAll            = "hive.logs.>"                // log entries (wildcard)
	SubjCapabilityRegister = "hive.capabilities.register" // capability registration
	SubjSecretsRequest     = "hive.secrets.request"       //nolint:gosec // NATS subject, not a credential
	SubjClusterState       = "hive.cluster.state.agents"  // cluster state replication
)
