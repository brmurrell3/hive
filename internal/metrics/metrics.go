// Package metrics provides a Prometheus-compatible metrics collector and HTTP
// handler for the Hive control plane. It hand-writes the Prometheus text
// exposition format without requiring any external Prometheus client library.
package metrics

import (
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
)

// StateReader is the interface used to snapshot current cluster state into
// metrics. It matches the read-only methods on *state.Store.
type StateReader interface {
	AllAgents() []*state.AgentState
	AllNodes() []*types.NodeState
}

const maxLatencyObservations = 10000

// latencyBucket holds observed latency values for computing quantiles.
// It uses a fixed-size ring buffer to bound memory usage.
type latencyBucket struct {
	values []float64
	pos    int  // next write position in the ring
	full   bool // true once the ring has wrapped around
}

// Collector tracks Hive metrics in memory. All methods are thread-safe.
type Collector struct {
	mu sync.Mutex

	// Gauges: agent counts by status.
	agentCounts map[string]int

	// Counters: NATS message counts by subject.
	messageCounts map[string]int64

	// Summary: capability invocation latency observations.
	latencies map[string]*latencyBucket

	// Gauges: heartbeat status per agent (true=healthy).
	heartbeats map[string]bool

	// Gauges: node resource usage.
	nodeMemory map[string]float64
	nodeCPU    map[string]float64
}

// NewCollector creates a new metrics collector.
func NewCollector() *Collector {
	return &Collector{
		agentCounts:   make(map[string]int),
		messageCounts: make(map[string]int64),
		latencies:     make(map[string]*latencyBucket),
		heartbeats:    make(map[string]bool),
		nodeMemory:    make(map[string]float64),
		nodeCPU:       make(map[string]float64),
	}
}

// SetAgentCount sets the number of agents with the given status.
func (c *Collector) SetAgentCount(status string, count int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.agentCounts[status] = count
}

// IncMessageCount increments the message counter for the given NATS subject.
func (c *Collector) IncMessageCount(subject string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.messageCounts[subject]++
}

// ObserveInvocationLatency records a capability invocation latency observation.
// Observations are stored in a fixed-size ring buffer (maxLatencyObservations)
// to prevent unbounded memory growth.
func (c *Collector) ObserveInvocationLatency(capability string, durationMs float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	bucket, ok := c.latencies[capability]
	if !ok {
		bucket = &latencyBucket{
			values: make([]float64, 0, maxLatencyObservations),
		}
		c.latencies[capability] = bucket
	}

	if len(bucket.values) < maxLatencyObservations {
		// Still filling up the ring buffer.
		bucket.values = append(bucket.values, durationMs)
	} else {
		// Ring buffer is full; overwrite the oldest entry.
		bucket.values[bucket.pos] = durationMs
		bucket.full = true
	}
	bucket.pos = (bucket.pos + 1) % maxLatencyObservations
}

// SetHeartbeatStatus records whether an agent is healthy.
func (c *Collector) SetHeartbeatStatus(agentID string, healthy bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heartbeats[agentID] = healthy
}

// SetNodeResourceUsage records memory and CPU usage percentages for a node.
func (c *Collector) SetNodeResourceUsage(nodeID string, memoryPercent, cpuPercent float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nodeMemory[nodeID] = memoryPercent
	c.nodeCPU[nodeID] = cpuPercent
}

// SnapshotFromStore computes agent count metrics from the current state store.
func (c *Collector) SnapshotFromStore(store StateReader) {
	agents := store.AllAgents()

	counts := make(map[string]int)
	for _, a := range agents {
		counts[string(a.Status)]++
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	c.agentCounts = counts
}

// Handler returns an http.Handler that serves metrics in Prometheus text
// exposition format at any path (typically mounted at /metrics).
func (c *Collector) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		output := c.render()
		fmt.Fprint(w, output)
	})
}

// render produces the full Prometheus text exposition output.
func (c *Collector) render() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	var b strings.Builder

	// --- hive_agents_total ---
	if len(c.agentCounts) > 0 {
		b.WriteString("# HELP hive_agents_total Number of agents by status\n")
		b.WriteString("# TYPE hive_agents_total gauge\n")
		for _, status := range sortedKeys(c.agentCounts) {
			fmt.Fprintf(&b, "hive_agents_total{status=%q} %d\n", status, c.agentCounts[status])
		}
	}

	// --- hive_nats_messages_total ---
	if len(c.messageCounts) > 0 {
		b.WriteString("# HELP hive_nats_messages_total Total NATS messages by subject\n")
		b.WriteString("# TYPE hive_nats_messages_total counter\n")
		for _, subject := range sortedKeysInt64(c.messageCounts) {
			fmt.Fprintf(&b, "hive_nats_messages_total{subject=%q} %d\n", subject, c.messageCounts[subject])
		}
	}

	// --- hive_capability_invocation_duration_ms ---
	if len(c.latencies) > 0 {
		b.WriteString("# HELP hive_capability_invocation_duration_ms Capability invocation latency in milliseconds\n")
		b.WriteString("# TYPE hive_capability_invocation_duration_ms summary\n")
		for _, cap := range sortedKeysLatency(c.latencies) {
			bucket := c.latencies[cap]
			if len(bucket.values) == 0 {
				continue
			}

			sorted := make([]float64, len(bucket.values))
			copy(sorted, bucket.values)
			sort.Float64s(sorted)

			q50 := quantile(sorted, 0.5)
			q90 := quantile(sorted, 0.9)
			q99 := quantile(sorted, 0.99)

			fmt.Fprintf(&b, "hive_capability_invocation_duration_ms{capability=%q,quantile=\"0.5\"} %s\n", cap, formatFloat(q50))
			fmt.Fprintf(&b, "hive_capability_invocation_duration_ms{capability=%q,quantile=\"0.9\"} %s\n", cap, formatFloat(q90))
			fmt.Fprintf(&b, "hive_capability_invocation_duration_ms{capability=%q,quantile=\"0.99\"} %s\n", cap, formatFloat(q99))

			var sum float64
			for _, v := range sorted {
				sum += v
			}
			fmt.Fprintf(&b, "hive_capability_invocation_duration_ms_sum{capability=%q} %s\n", cap, formatFloat(sum))
			fmt.Fprintf(&b, "hive_capability_invocation_duration_ms_count{capability=%q} %d\n", cap, len(sorted))
		}
	}

	// --- hive_heartbeat_healthy ---
	if len(c.heartbeats) > 0 {
		b.WriteString("# HELP hive_heartbeat_healthy Whether an agent heartbeat is healthy (1) or not (0)\n")
		b.WriteString("# TYPE hive_heartbeat_healthy gauge\n")
		for _, agentID := range sortedKeysBool(c.heartbeats) {
			val := 0
			if c.heartbeats[agentID] {
				val = 1
			}
			fmt.Fprintf(&b, "hive_heartbeat_healthy{agent_id=%q} %d\n", agentID, val)
		}
	}

	// --- hive_node_memory_usage_percent ---
	if len(c.nodeMemory) > 0 {
		b.WriteString("# HELP hive_node_memory_usage_percent Node memory usage percentage\n")
		b.WriteString("# TYPE hive_node_memory_usage_percent gauge\n")
		for _, nodeID := range sortedKeysFloat64(c.nodeMemory) {
			fmt.Fprintf(&b, "hive_node_memory_usage_percent{node_id=%q} %s\n", nodeID, formatFloat(c.nodeMemory[nodeID]))
		}
	}

	// --- hive_node_cpu_usage_percent ---
	if len(c.nodeCPU) > 0 {
		b.WriteString("# HELP hive_node_cpu_usage_percent Node CPU usage percentage\n")
		b.WriteString("# TYPE hive_node_cpu_usage_percent gauge\n")
		for _, nodeID := range sortedKeysFloat64(c.nodeCPU) {
			fmt.Fprintf(&b, "hive_node_cpu_usage_percent{node_id=%q} %s\n", nodeID, formatFloat(c.nodeCPU[nodeID]))
		}
	}

	return b.String()
}

// quantile computes the q-th quantile from sorted values using nearest-rank.
func quantile(sorted []float64, q float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}

	rank := q * float64(len(sorted)-1)
	lower := int(math.Floor(rank))
	upper := int(math.Ceil(rank))

	if lower == upper {
		return sorted[lower]
	}

	// Linear interpolation.
	frac := rank - float64(lower)
	return sorted[lower]*(1-frac) + sorted[upper]*frac
}

// formatFloat formats a float for Prometheus output, avoiding trailing zeros
// but keeping at least one decimal place.
func formatFloat(v float64) string {
	s := fmt.Sprintf("%.6g", v)
	return s
}

// --- sorted key helpers for deterministic output ---

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeysInt64(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeysFloat64(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeysBool(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func sortedKeysLatency(m map[string]*latencyBucket) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
