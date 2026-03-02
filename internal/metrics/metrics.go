// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package metrics provides Prometheus metrics for the Hive control plane
// using the official prometheus/client_golang library.
package metrics

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const labelOther = "other"

// knownErrorTypes is the bounded set of error types tracked individually.
// Any error type not in this set is bucketed under "other" to prevent unbounded
// label cardinality from causing memory growth.
var knownErrorTypes = map[string]bool{
	"timeout":           true,
	"connection":        true,
	"publish":           true,
	"subscribe":         true,
	"marshal":           true,
	"unmarshal":         true,
	"validation":        true,
	"auth":              true,
	"not_found":         true,
	"internal":          true,
	"resource_exceeded": true,
	"vm":                true,
	"capability":        true,
	"health":            true,
	"config":            true,
}

// normalizeErrorType maps an error type to a bounded label value. Known
// error types are preserved; unknown types are mapped to "other".
func normalizeErrorType(errorType string) string {
	if knownErrorTypes[errorType] {
		return errorType
	}
	return labelOther
}

// knownSubjects is the bounded set of NATS subjects tracked individually.
// Any subject not in this set is bucketed under "other" to prevent unbounded
// label cardinality from causing memory growth.
var knownSubjects = map[string]bool{
	"hive.health":     true,
	"hive.tasks":      true,
	"hive.logs":       true,
	"hive.capability": true,
	"hive.join":       true,
	"hive.heartbeat":  true,
	"hive.ctl":        true,
	"hive.events":     true,
	"hive.firmware":   true,
	"hive.mqtt":       true,
	"hive.cost":       true,
	"hive.pipeline":   true,
	"hive.secrets":    true,
	"hive.director":   true,
}

// normalizeSubject maps a NATS subject to a bounded label value. Known
// top-level prefixes are preserved; unknown subjects are mapped to "other".
func normalizeSubject(subject string) string {
	// Check exact match first.
	if knownSubjects[subject] {
		return subject
	}

	// Check if the subject starts with any known prefix (e.g., "hive.logs.agent-1" -> "hive.logs").
	for known := range knownSubjects {
		if strings.HasPrefix(subject, known+".") || strings.HasPrefix(subject, known+">") {
			return known
		}
	}

	return labelOther
}

// StateReader is the interface used to snapshot current cluster state into
// metrics. It matches the read-only methods on *state.Store.
type StateReader interface {
	AllAgents() []*state.AgentState
	AllNodes() []*types.NodeState
}

// Collector tracks Hive metrics using Prometheus client_golang. All methods are thread-safe.
type Collector struct {
	registry *prometheus.Registry

	agentsTotal    *prometheus.GaugeVec
	messagesTotal  *prometheus.CounterVec
	latencySummary *prometheus.SummaryVec
	heartbeat      *prometheus.GaugeVec
	nodeMemory     *prometheus.GaugeVec
	nodeCPU        *prometheus.GaugeVec
	errorsTotal    *prometheus.CounterVec
	natsConnected  prometheus.Gauge

	// knownAgents tracks which agent_id labels have been set in the
	// heartbeat gauge, enabling cleanup of stale entries.
	mu          sync.Mutex
	knownAgents map[string]struct{}

	// knownNodes tracks which node_id labels have been set in
	// nodeMemory and nodeCPU gauges, enabling cleanup of stale entries.
	nodeMu     sync.Mutex
	knownNodes map[string]struct{}

	// knownCapabilities is the bounded set of capability names tracked
	// individually in latency metrics. Unknown capabilities are bucketed
	// under "other" to prevent unbounded label cardinality.
	capMu               sync.RWMutex
	knownCapabilities   map[string]struct{}
	droppedCapabilities map[string]struct{} // tracks capability names already warned about

	logger *slog.Logger
}

// NewCollector creates a new metrics collector with all Hive metrics registered.
// An optional logger can be passed; if nil, slog.Default() is used.
func NewCollector(opts ...func(*Collector)) *Collector {
	reg := prometheus.NewRegistry()

	agentsTotal := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_agents_total",
		Help: "Number of agents by status",
	}, []string{"status"})

	messagesTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hive_nats_messages_total",
		Help: "Total NATS messages by subject",
	}, []string{"subject"})

	// Consider switching to HistogramVec for cross-instance aggregation in future.
	latencySummary := prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Name: "hive_capability_invocation_duration_ms",
		Help: "Capability invocation latency in milliseconds",
		Objectives: map[float64]float64{
			0.5:  0.05,
			0.9:  0.01,
			0.99: 0.001,
		},
	}, []string{"capability"})

	heartbeat := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_heartbeat_healthy",
		Help: "Whether an agent heartbeat is healthy (1) or not (0)",
	}, []string{"agent_id"})

	nodeMemory := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_node_memory_usage_percent",
		Help: "Node memory usage percentage",
	}, []string{"node_id"})

	nodeCPU := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_node_cpu_usage_percent",
		Help: "Node CPU usage percentage",
	}, []string{"node_id"})

	errorsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hive_errors_total",
		Help: "Total error count by error type",
	}, []string{"error_type"})

	natsConnected := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "hive_nats_connected",
		Help: "Whether the NATS connection is active (1) or not (0)",
	})

	reg.MustRegister(agentsTotal, messagesTotal, latencySummary, heartbeat, nodeMemory, nodeCPU, errorsTotal, natsConnected)

	c := &Collector{
		registry:            reg,
		agentsTotal:         agentsTotal,
		messagesTotal:       messagesTotal,
		latencySummary:      latencySummary,
		heartbeat:           heartbeat,
		nodeMemory:          nodeMemory,
		nodeCPU:             nodeCPU,
		errorsTotal:         errorsTotal,
		natsConnected:       natsConnected,
		knownAgents:         make(map[string]struct{}),
		knownNodes:          make(map[string]struct{}),
		knownCapabilities:   make(map[string]struct{}),
		droppedCapabilities: make(map[string]struct{}),
		logger:              slog.Default(),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// WithLogger returns an option that sets the logger on a Collector.
func WithLogger(logger *slog.Logger) func(*Collector) {
	return func(c *Collector) {
		if logger != nil {
			c.logger = logger
		}
	}
}

// knownAgentStatuses is the bounded set of agent statuses tracked individually.
// Unknown statuses are bucketed under "other" to prevent unbounded label cardinality.
var knownAgentStatuses = map[string]bool{
	"running":  true,
	"stopped":  true,
	"failed":   true,
	"creating": true,
	"starting": true,
	"stopping": true,
	"pending":  true,
	"unknown":  true,
}

// normalizeAgentStatus maps an agent status to a bounded label value.
func normalizeAgentStatus(status string) string {
	lower := strings.ToLower(status)
	if knownAgentStatuses[lower] {
		return lower
	}
	return labelOther
}

// SetAgentCount sets the number of agents with the given status.
// Unknown statuses are normalized to "other" to prevent unbounded label cardinality.
func (c *Collector) SetAgentCount(status string, count int) {
	c.agentsTotal.With(prometheus.Labels{"status": normalizeAgentStatus(status)}).Set(float64(count))
}

// IncMessageCount increments the message counter for the given NATS subject.
// Subjects are normalized to a bounded set of known prefixes to prevent
// unbounded label cardinality. Unknown subjects are bucketed under "other".
func (c *Collector) IncMessageCount(subject string) {
	c.messagesTotal.With(prometheus.Labels{"subject": normalizeSubject(subject)}).Inc()
}

// maxKnownCapabilities is the upper bound on unique capability label values
// tracked in latency metrics. Beyond this limit, new capabilities are shed
// to prevent unbounded cardinality from memory growth.
const maxKnownCapabilities = 10000

// RegisterCapability adds a capability name to the known set so that it is
// tracked as its own label in latency metrics. Capabilities not in the known
// set are bucketed under "other" to prevent unbounded cardinality.
func (c *Collector) RegisterCapability(name string) {
	normalized := normalizeCapabilityName(name)
	c.capMu.Lock()
	if len(c.knownCapabilities) >= maxKnownCapabilities {
		// Log a warning once per dropped capability name.
		if _, alreadyWarned := c.droppedCapabilities[normalized]; !alreadyWarned {
			c.droppedCapabilities[normalized] = struct{}{}
			c.capMu.Unlock()
			c.logger.Warn("capability dropped from metrics tracking: known capabilities limit reached",
				"capability", normalized,
				"limit", maxKnownCapabilities,
			)
			return
		}
		c.capMu.Unlock()
		return
	}
	c.knownCapabilities[normalized] = struct{}{}
	c.capMu.Unlock()
}

// RemoveCapability cleans up a capability from the known set when it is
// no longer registered, preventing unbounded label growth. It also deletes
// the stale Prometheus time series for the capability.
func (c *Collector) RemoveCapability(name string) {
	normalized := normalizeCapabilityName(name)
	c.capMu.Lock()
	delete(c.knownCapabilities, normalized)
	c.capMu.Unlock()
	c.latencySummary.Delete(prometheus.Labels{"capability": normalized})
}

// normalizeCapabilityName lowercases and trims a capability name for
// consistent label matching.
func normalizeCapabilityName(name string) string {
	return strings.TrimSpace(strings.ToLower(name))
}

// normalizeCapability maps a capability name to a bounded label value.
// Known capabilities (registered via RegisterCapability) are preserved;
// unknown names are mapped to "other".
func (c *Collector) normalizeCapability(capability string) string {
	normalized := normalizeCapabilityName(capability)
	c.capMu.RLock()
	_, ok := c.knownCapabilities[normalized]
	c.capMu.RUnlock()
	if ok {
		return normalized
	}
	return labelOther
}

// ObserveInvocationLatency records a capability invocation latency observation.
// Capability names are normalized to a bounded set of known capabilities
// (registered via RegisterCapability). Unknown capabilities are bucketed
// under "other" to prevent unbounded label cardinality.
func (c *Collector) ObserveInvocationLatency(capability string, durationMs float64) {
	c.latencySummary.With(prometheus.Labels{"capability": c.normalizeCapability(capability)}).Observe(durationMs)
}

// maxKnownAgents is the upper bound on unique agent_id label values tracked
// in heartbeat metrics. Beyond this limit, new agent IDs are shed to prevent
// unbounded cardinality from memory growth.
const maxKnownAgents = 10000

// SetHeartbeatStatus records whether an agent is healthy.
func (c *Collector) SetHeartbeatStatus(agentID string, healthy bool) {
	c.mu.Lock()
	_, known := c.knownAgents[agentID]
	if !known {
		if len(c.knownAgents) >= maxKnownAgents {
			c.mu.Unlock()
			return // shed to prevent unbounded cardinality
		}
		c.knownAgents[agentID] = struct{}{}
	}
	c.mu.Unlock()

	val := float64(0)
	if healthy {
		val = 1
	}
	// Prometheus GaugeVec.With().Set() is goroutine-safe; call outside lock
	// to avoid holding the mutex during the Prometheus API call.
	// Only reached for agents that are (or were just) registered in knownAgents.
	c.heartbeat.With(prometheus.Labels{"agent_id": agentID}).Set(val)
}

// RemoveAgent cleans up stale heartbeat label values when an agent is removed.
func (c *Collector) RemoveAgent(agentID string) {
	c.mu.Lock()
	_, exists := c.knownAgents[agentID]
	if exists {
		delete(c.knownAgents, agentID)
	}
	c.mu.Unlock()

	// Prometheus Delete is goroutine-safe; call outside lock to avoid holding
	// the mutex during the Prometheus API call.
	if exists {
		c.heartbeat.Delete(prometheus.Labels{"agent_id": agentID})
	}
}

// IncErrorCount increments the error counter for the given error type.
// Error types are normalized to a bounded set of known types to prevent
// unbounded label cardinality. Unknown types are bucketed under "other".
func (c *Collector) IncErrorCount(errorType string) {
	c.errorsTotal.With(prometheus.Labels{"error_type": normalizeErrorType(errorType)}).Inc()
}

// SetNATSConnected records whether the NATS connection is active.
func (c *Collector) SetNATSConnected(connected bool) {
	val := float64(0)
	if connected {
		val = 1
	}
	c.natsConnected.Set(val)
}

// maxKnownNodes is the upper bound on unique node_id label values tracked
// in node resource metrics. Beyond this limit, new node IDs are shed to
// prevent unbounded cardinality from memory growth.
const maxKnownNodes = 10000

// SetNodeResourceUsage records memory and CPU usage percentages for a node.
func (c *Collector) SetNodeResourceUsage(nodeID string, memoryPercent, cpuPercent float64) {
	c.nodeMu.Lock()
	if _, exists := c.knownNodes[nodeID]; !exists {
		if len(c.knownNodes) >= maxKnownNodes {
			c.nodeMu.Unlock()
			return // shed to prevent unbounded cardinality
		}
		c.knownNodes[nodeID] = struct{}{}
	}
	c.nodeMu.Unlock()

	// Prometheus GaugeVec.With().Set() is goroutine-safe; call outside lock
	// to avoid holding the mutex during the Prometheus API call.
	// Only reached for nodes that are (or were just) registered in knownNodes.
	c.nodeMemory.With(prometheus.Labels{"node_id": nodeID}).Set(memoryPercent)
	c.nodeCPU.With(prometheus.Labels{"node_id": nodeID}).Set(cpuPercent)
}

// RemoveNode cleans up stale node metric labels when a node is removed,
// preventing unbounded label growth in nodeMemory and nodeCPU gauge vecs.
func (c *Collector) RemoveNode(nodeID string) {
	c.nodeMu.Lock()
	_, exists := c.knownNodes[nodeID]
	if exists {
		delete(c.knownNodes, nodeID)
	}
	c.nodeMu.Unlock()

	// Prometheus Delete is goroutine-safe; call outside lock to avoid holding
	// the mutex during the Prometheus API call.
	if exists {
		c.nodeMemory.Delete(prometheus.Labels{"node_id": nodeID})
		c.nodeCPU.Delete(prometheus.Labels{"node_id": nodeID})
	}
}

// SnapshotFromStore computes agent count metrics from the current state store.
// It tracks which status labels were set this round and deletes stale ones
// that no longer have any agents, avoiding the Reset()-then-set pattern which
// creates a brief window where metrics disappear.
func (c *Collector) SnapshotFromStore(store StateReader) {
	agents := store.AllAgents()

	counts := make(map[string]int)
	for _, a := range agents {
		counts[string(a.Status)]++
	}

	// All known agent statuses that might have been set previously.
	allStatuses := []string{
		string(state.AgentStatusRunning),
		string(state.AgentStatusStopped),
		string(state.AgentStatusFailed),
		string(state.AgentStatusPending),
		string(state.AgentStatusCreating),
		string(state.AgentStatusStarting),
		string(state.AgentStatusStopping),
	}

	// Set current counts directly (overwrites previous values).
	for status, count := range counts {
		c.agentsTotal.With(prometheus.Labels{"status": status}).Set(float64(count))
	}

	// Delete label values for statuses that have zero agents this round.
	for _, status := range allStatuses {
		if _, exists := counts[status]; !exists {
			c.agentsTotal.Delete(prometheus.Labels{"status": status})
		}
	}

	// Clean up heartbeat gauges for agents that no longer exist.
	activeAgents := make(map[string]struct{}, len(agents))
	for _, a := range agents {
		activeAgents[a.ID] = struct{}{}
	}

	c.mu.Lock()
	for agentID := range c.knownAgents {
		if _, exists := activeAgents[agentID]; !exists {
			c.heartbeat.Delete(prometheus.Labels{"agent_id": agentID})
			delete(c.knownAgents, agentID)
		}
	}
	c.mu.Unlock()

	// Clean up node resource gauges for nodes that no longer exist.
	nodes := store.AllNodes()
	activeNodes := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		activeNodes[n.ID] = struct{}{}
	}

	c.nodeMu.Lock()
	for nodeID := range c.knownNodes {
		if _, exists := activeNodes[nodeID]; !exists {
			c.nodeMemory.Delete(prometheus.Labels{"node_id": nodeID})
			c.nodeCPU.Delete(prometheus.Labels{"node_id": nodeID})
			delete(c.knownNodes, nodeID)
		}
	}
	c.nodeMu.Unlock()
}

// Handler returns an http.Handler that serves metrics in Prometheus text
// exposition format (typically mounted at /metrics).
func (c *Collector) Handler() http.Handler {
	return promhttp.HandlerFor(c.registry, promhttp.HandlerOpts{})
}
