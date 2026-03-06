// Package metrics provides Prometheus metrics for the Hive control plane
// using the official prometheus/client_golang library.
package metrics

import (
	"net/http"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

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
}

// NewCollector creates a new metrics collector with all Hive metrics registered.
func NewCollector() *Collector {
	reg := prometheus.NewRegistry()

	agentsTotal := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hive_agents_total",
		Help: "Number of agents by status",
	}, []string{"status"})

	messagesTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hive_nats_messages_total",
		Help: "Total NATS messages by subject",
	}, []string{"subject"})

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

	reg.MustRegister(agentsTotal, messagesTotal, latencySummary, heartbeat, nodeMemory, nodeCPU)

	return &Collector{
		registry:       reg,
		agentsTotal:    agentsTotal,
		messagesTotal:  messagesTotal,
		latencySummary: latencySummary,
		heartbeat:      heartbeat,
		nodeMemory:     nodeMemory,
		nodeCPU:        nodeCPU,
	}
}

// SetAgentCount sets the number of agents with the given status.
func (c *Collector) SetAgentCount(status string, count int) {
	c.agentsTotal.With(prometheus.Labels{"status": status}).Set(float64(count))
}

// IncMessageCount increments the message counter for the given NATS subject.
func (c *Collector) IncMessageCount(subject string) {
	c.messagesTotal.With(prometheus.Labels{"subject": subject}).Inc()
}

// ObserveInvocationLatency records a capability invocation latency observation.
func (c *Collector) ObserveInvocationLatency(capability string, durationMs float64) {
	c.latencySummary.With(prometheus.Labels{"capability": capability}).Observe(durationMs)
}

// SetHeartbeatStatus records whether an agent is healthy.
func (c *Collector) SetHeartbeatStatus(agentID string, healthy bool) {
	val := float64(0)
	if healthy {
		val = 1
	}
	c.heartbeat.With(prometheus.Labels{"agent_id": agentID}).Set(val)
}

// SetNodeResourceUsage records memory and CPU usage percentages for a node.
func (c *Collector) SetNodeResourceUsage(nodeID string, memoryPercent, cpuPercent float64) {
	c.nodeMemory.With(prometheus.Labels{"node_id": nodeID}).Set(memoryPercent)
	c.nodeCPU.With(prometheus.Labels{"node_id": nodeID}).Set(cpuPercent)
}

// SnapshotFromStore computes agent count metrics from the current state store.
func (c *Collector) SnapshotFromStore(store StateReader) {
	agents := store.AllAgents()

	counts := make(map[string]int)
	for _, a := range agents {
		counts[string(a.Status)]++
	}

	// Reset all status gauges, then set the current counts.
	c.agentsTotal.Reset()
	for status, count := range counts {
		c.agentsTotal.With(prometheus.Labels{"status": status}).Set(float64(count))
	}
}

// Handler returns an http.Handler that serves metrics in Prometheus text
// exposition format (typically mounted at /metrics).
func (c *Collector) Handler() http.Handler {
	return promhttp.HandlerFor(c.registry, promhttp.HandlerOpts{})
}
