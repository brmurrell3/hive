//go:build unit

package metrics

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
)

// mockStateReader implements StateReader for testing.
type mockStateReader struct {
	agents []*state.AgentState
	nodes  []*types.NodeState
}

func (m *mockStateReader) AllAgents() []*state.AgentState { return m.agents }
func (m *mockStateReader) AllNodes() []*types.NodeState   { return m.nodes }

func TestCollector_AgentCount(t *testing.T) {
	t.Helper()

	tests := []struct {
		name     string
		counts   map[string]int
		contains []string
	}{
		{
			name: "single status",
			counts: map[string]int{
				"RUNNING": 5,
			},
			contains: []string{
				`hive_agents_total{status="RUNNING"} 5`,
			},
		},
		{
			name: "multiple statuses",
			counts: map[string]int{
				"RUNNING": 3,
				"STOPPED": 2,
				"FAILED":  1,
			},
			contains: []string{
				`hive_agents_total{status="RUNNING"} 3`,
				`hive_agents_total{status="STOPPED"} 2`,
				`hive_agents_total{status="FAILED"} 1`,
			},
		},
		{
			name: "zero count",
			counts: map[string]int{
				"RUNNING": 0,
			},
			contains: []string{
				`hive_agents_total{status="RUNNING"} 0`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCollector()
			for status, count := range tt.counts {
				c.SetAgentCount(status, count)
			}

			output := c.render()

			for _, want := range tt.contains {
				if !strings.Contains(output, want) {
					t.Errorf("output missing %q\ngot:\n%s", want, output)
				}
			}

			if !strings.Contains(output, "# HELP hive_agents_total") {
				t.Error("output missing HELP line for hive_agents_total")
			}
			if !strings.Contains(output, "# TYPE hive_agents_total gauge") {
				t.Error("output missing TYPE line for hive_agents_total")
			}
		})
	}
}

func TestCollector_MessageCount(t *testing.T) {
	tests := []struct {
		name     string
		subjects []string
		contains []string
	}{
		{
			name:     "single subject",
			subjects: []string{"hive.health", "hive.health", "hive.health"},
			contains: []string{
				`hive_nats_messages_total{subject="hive.health"} 3`,
			},
		},
		{
			name:     "multiple subjects",
			subjects: []string{"hive.health", "hive.tasks", "hive.health"},
			contains: []string{
				`hive_nats_messages_total{subject="hive.health"} 2`,
				`hive_nats_messages_total{subject="hive.tasks"} 1`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := NewCollector()
			for _, subject := range tt.subjects {
				c.IncMessageCount(subject)
			}

			output := c.render()

			for _, want := range tt.contains {
				if !strings.Contains(output, want) {
					t.Errorf("output missing %q\ngot:\n%s", want, output)
				}
			}

			if !strings.Contains(output, "# TYPE hive_nats_messages_total counter") {
				t.Error("output missing TYPE line for hive_nats_messages_total")
			}
		})
	}
}

func TestCollector_InvocationLatency(t *testing.T) {
	c := NewCollector()

	// Add some observations.
	values := []float64{100, 200, 150, 120, 180, 300, 450, 110, 130, 160}
	for _, v := range values {
		c.ObserveInvocationLatency("answer-questions", v)
	}

	output := c.render()

	// Check that quantile lines are present.
	if !strings.Contains(output, `capability="answer-questions",quantile="0.5"`) {
		t.Errorf("output missing p50 quantile line\ngot:\n%s", output)
	}
	if !strings.Contains(output, `capability="answer-questions",quantile="0.9"`) {
		t.Errorf("output missing p90 quantile line\ngot:\n%s", output)
	}
	if !strings.Contains(output, `capability="answer-questions",quantile="0.99"`) {
		t.Errorf("output missing p99 quantile line\ngot:\n%s", output)
	}

	// Check sum and count lines.
	if !strings.Contains(output, `hive_capability_invocation_duration_ms_count{capability="answer-questions"} 10`) {
		t.Errorf("output missing count line\ngot:\n%s", output)
	}
	if !strings.Contains(output, `hive_capability_invocation_duration_ms_sum{capability="answer-questions"}`) {
		t.Errorf("output missing sum line\ngot:\n%s", output)
	}

	if !strings.Contains(output, "# TYPE hive_capability_invocation_duration_ms summary") {
		t.Error("output missing TYPE line for summary")
	}
}

func TestCollector_PrometheusFormat(t *testing.T) {
	c := NewCollector()

	c.SetAgentCount("RUNNING", 5)
	c.SetAgentCount("STOPPED", 2)
	c.IncMessageCount("hive.health")
	c.ObserveInvocationLatency("search", 100.5)
	c.SetHeartbeatStatus("agent-1", true)
	c.SetHeartbeatStatus("agent-2", false)
	c.SetNodeResourceUsage("node-1", 65.2, 42.1)

	output := c.render()

	// Validate that every non-empty line is either a comment (# ...) or a
	// metric line matching: metric_name{labels} value or metric_name value.
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			// Comment line (HELP or TYPE).
			if !strings.HasPrefix(line, "# HELP ") && !strings.HasPrefix(line, "# TYPE ") {
				t.Errorf("line %d: unexpected comment format: %s", i+1, line)
			}
			continue
		}

		// Metric line: name{labels} value  or  name value
		// Must contain at least one space separating name from value.
		parts := strings.Fields(line)
		if len(parts) < 2 {
			t.Errorf("line %d: metric line has fewer than 2 fields: %s", i+1, line)
		}
	}

	// Verify the HTTP handler sets the correct Content-Type.
	handler := c.Handler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	resp := rec.Result()
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("unexpected Content-Type: %s", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	if len(body) == 0 {
		t.Error("response body is empty")
	}

	// Check specific metrics are in the output.
	bodyStr := string(body)
	checks := []string{
		"hive_agents_total",
		"hive_nats_messages_total",
		"hive_capability_invocation_duration_ms",
		"hive_heartbeat_healthy",
		"hive_node_memory_usage_percent",
		"hive_node_cpu_usage_percent",
	}
	for _, check := range checks {
		if !strings.Contains(bodyStr, check) {
			t.Errorf("response body missing metric %q", check)
		}
	}
}

func TestCollector_SnapshotFromStore(t *testing.T) {
	c := NewCollector()
	store := &mockStateReader{
		agents: []*state.AgentState{
			{ID: "a1", Status: state.AgentStatusRunning},
			{ID: "a2", Status: state.AgentStatusRunning},
			{ID: "a3", Status: state.AgentStatusStopped},
			{ID: "a4", Status: state.AgentStatusFailed},
		},
	}

	c.SnapshotFromStore(store)
	output := c.render()

	if !strings.Contains(output, `hive_agents_total{status="RUNNING"} 2`) {
		t.Errorf("expected 2 RUNNING agents in snapshot\ngot:\n%s", output)
	}
	if !strings.Contains(output, `hive_agents_total{status="STOPPED"} 1`) {
		t.Errorf("expected 1 STOPPED agent in snapshot\ngot:\n%s", output)
	}
	if !strings.Contains(output, `hive_agents_total{status="FAILED"} 1`) {
		t.Errorf("expected 1 FAILED agent in snapshot\ngot:\n%s", output)
	}
}

func TestCollector_HeartbeatStatus(t *testing.T) {
	c := NewCollector()
	c.SetHeartbeatStatus("agent-1", true)
	c.SetHeartbeatStatus("agent-2", false)

	output := c.render()

	if !strings.Contains(output, `hive_heartbeat_healthy{agent_id="agent-1"} 1`) {
		t.Errorf("expected agent-1 healthy=1\ngot:\n%s", output)
	}
	if !strings.Contains(output, `hive_heartbeat_healthy{agent_id="agent-2"} 0`) {
		t.Errorf("expected agent-2 healthy=0\ngot:\n%s", output)
	}
}

func TestCollector_NodeResourceUsage(t *testing.T) {
	c := NewCollector()
	c.SetNodeResourceUsage("node-1", 65.2, 42.1)

	output := c.render()

	if !strings.Contains(output, `hive_node_memory_usage_percent{node_id="node-1"}`) {
		t.Errorf("expected node-1 memory metric\ngot:\n%s", output)
	}
	if !strings.Contains(output, `hive_node_cpu_usage_percent{node_id="node-1"}`) {
		t.Errorf("expected node-1 cpu metric\ngot:\n%s", output)
	}
}
