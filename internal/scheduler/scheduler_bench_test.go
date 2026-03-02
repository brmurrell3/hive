// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package scheduler

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
)

// newBenchStore creates a state store for benchmarks using b.TempDir().
func newBenchStore(b *testing.B) *state.Store {
	b.Helper()
	dir := b.TempDir()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := state.NewStore(filepath.Join(dir, "state.db"), logger)
	if err != nil {
		b.Fatalf("creating store: %v", err)
	}
	return store
}

func benchLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// addBenchNode adds a fully-capable Tier 1 node with KVM to the store.
func addBenchNode(b *testing.B, store *state.Store, id string, memTotal int64, cpuCount int) {
	b.Helper()
	node := &types.NodeState{
		ID:   id,
		Tier: types.NodeTier1,
		Arch: "amd64",
		Resources: types.NodeResources{
			MemoryTotal: memTotal,
			CPUCount:    cpuCount,
			KVMAvail:    true,
		},
		Status:   types.NodeStatusOnline,
		Labels:   map[string]string{},
		JoinedAt: time.Now(),
	}
	if err := store.SetNode(node); err != nil {
		b.Fatalf("SetNode %s: %v", id, err)
	}
}

// benchManifest builds an AgentManifest for benchmarks.
func benchManifest(id, team, memory string, vcpus int) *types.AgentManifest {
	return &types.AgentManifest{
		APIVersion: "hive/v1",
		Kind:       "Agent",
		Metadata: types.AgentMetadata{
			ID:   id,
			Team: team,
		},
		Spec: types.AgentSpec{
			Runtime: types.AgentRuntime{Type: "openclaw"},
			Resources: types.AgentResources{
				Memory: memory,
				VCPUs:  vcpus,
			},
		},
	}
}

// BenchmarkSchedule benchmarks the Schedule hot path with varying node counts.
// Each sub-benchmark sets up N nodes and then calls Schedule repeatedly for
// fresh agent IDs. The scheduler rebuilds allocations on each call (dirty flag),
// so this exercises the full scoring path including AllNodes/AllAgents reads.
func BenchmarkSchedule(b *testing.B) {
	nodeCounts := []int{10, 100, 1000}

	for _, n := range nodeCounts {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			store := newBenchStore(b)
			defer store.Close()

			// Populate nodes: each has 16 GiB and 16 vCPUs.
			const memPerNode = 16 * 1024 * 1024 * 1024
			const cpuPerNode = 16
			for i := 0; i < n; i++ {
				addBenchNode(b, store, fmt.Sprintf("node-%04d", i), memPerNode, cpuPerNode)
			}

			sched := NewScheduler(store, benchLogger())

			// Each iteration gets a unique agent ID to avoid state accumulation
			// issues; the scheduler will try to allocate a 512 MiB agent each time.
			// We pre-generate IDs outside the timed region.
			ids := make([]string, b.N)
			for i := range ids {
				ids[i] = fmt.Sprintf("agent-%08d", i)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				manifest := benchManifest(ids[i], "", "512Mi", 1)
				if _, err := sched.Schedule(manifest); err != nil {
					b.Fatalf("Schedule failed: %v", err)
				}
			}
		})
	}
}

// BenchmarkScheduleScoring isolates the scoring path by using a fresh scheduler
// (dirty=false after first rebuild) so that rebuildAllocations is a no-op.
// This more directly benchmarks the node filtering + scoreAndSelect logic.
func BenchmarkScheduleScoring(b *testing.B) {
	nodeCounts := []int{10, 100, 1000}

	for _, n := range nodeCounts {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			store := newBenchStore(b)
			defer store.Close()

			const memPerNode = 32 * 1024 * 1024 * 1024 // 32 GiB – lots of room
			const cpuPerNode = 32
			for i := 0; i < n; i++ {
				addBenchNode(b, store, fmt.Sprintf("node-%04d", i), memPerNode, cpuPerNode)
			}

			sched := NewScheduler(store, benchLogger())
			// Trigger an initial Schedule to warm up allocations and clear dirty.
			warmup := benchManifest("warmup-agent", "", "128Mi", 1)
			if _, err := sched.Schedule(warmup); err != nil {
				b.Fatalf("warmup Schedule failed: %v", err)
			}

			ids := make([]string, b.N)
			for i := range ids {
				ids[i] = fmt.Sprintf("bench-agent-%08d", i)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				manifest := benchManifest(ids[i], "", "128Mi", 1)
				if _, err := sched.Schedule(manifest); err != nil {
					b.Fatalf("Schedule failed: %v", err)
				}
			}
		})
	}
}

// BenchmarkRebuildAllocations benchmarks rebuildAllocations directly by calling
// SyncAllocations (which sets dirty=true) before each Schedule, forcing a full
// rebuild from store. This exercises the store read + allocation map construction.
func BenchmarkRebuildAllocations(b *testing.B) {
	nodeCounts := []int{10, 100, 1000}

	for _, n := range nodeCounts {
		b.Run(fmt.Sprintf("nodes=%d", n), func(b *testing.B) {
			store := newBenchStore(b)
			defer store.Close()

			const memPerNode = 64 * 1024 * 1024 * 1024 // 64 GiB
			const cpuPerNode = 64
			for i := 0; i < n; i++ {
				addBenchNode(b, store, fmt.Sprintf("node-%04d", i), memPerNode, cpuPerNode)
			}

			// Also populate some agents so AllAgents has work to do.
			agentCount := n / 2
			if agentCount < 1 {
				agentCount = 1
			}
			for i := 0; i < agentCount; i++ {
				agentID := fmt.Sprintf("existing-%04d", i)
				nodeID := fmt.Sprintf("node-%04d", i%n)
				if err := store.SetAgent(&state.AgentState{
					ID:          agentID,
					Status:      state.AgentStatusRunning,
					NodeID:      nodeID,
					MemoryBytes: 512 * 1024 * 1024,
					VCPUs:       1,
				}); err != nil {
					b.Fatalf("SetAgent: %v", err)
				}
			}

			sched := NewScheduler(store, benchLogger())

			ids := make([]string, b.N)
			for i := range ids {
				ids[i] = fmt.Sprintf("bench-agent-%08d", i)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Force a full rebuild every iteration.
				sched.SyncAllocations()
				manifest := benchManifest(ids[i], "", "512Mi", 1)
				if _, err := sched.Schedule(manifest); err != nil {
					b.Fatalf("Schedule failed: %v", err)
				}
			}
		})
	}
}
