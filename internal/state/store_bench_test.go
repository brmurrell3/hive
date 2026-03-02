// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

//go:build unit

package state

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newBenchStore creates a state store for benchmarks.
func newBenchStore(b *testing.B) *Store {
	b.Helper()
	dir := b.TempDir()
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := NewStore(filepath.Join(dir, "state.db"), logger)
	if err != nil {
		b.Fatalf("NewStore: %v", err)
	}
	b.Cleanup(func() { store.Close() })
	return store
}

// seedAgents populates the store with n agents in RUNNING status.
// Agent IDs are formatted as "agent-%04d".
func seedAgents(b *testing.B, store *Store, n int) {
	b.Helper()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("agent-%04d", i)
		if err := store.SetAgent(&AgentState{
			ID:             id,
			Team:           "bench-team",
			Status:         AgentStatusRunning,
			NodeID:         fmt.Sprintf("node-%04d", i%10),
			MemoryBytes:    512 * 1024 * 1024,
			VCPUs:          1,
			LastTransition: time.Now(),
		}); err != nil {
			b.Fatalf("seeding agent %s: %v", id, err)
		}
	}
}

// BenchmarkGetAgent benchmarks the read path for a single agent lookup.
// Sub-benchmarks vary the total number of agents in the store to capture
// any map lookup or copy overhead as the state grows.
func BenchmarkGetAgent(b *testing.B) {
	agentCounts := []int{10, 100, 1000}

	for _, n := range agentCounts {
		b.Run(fmt.Sprintf("agents=%d", n), func(b *testing.B) {
			store := newBenchStore(b)
			seedAgents(b, store, n)

			// Always look up the middle agent so we exercise a typical case.
			targetID := fmt.Sprintf("agent-%04d", n/2)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if store.GetAgent(targetID) == nil {
					b.Fatalf("GetAgent returned nil for %s", targetID)
				}
			}
		})
	}
}

// BenchmarkGetAgentParallel benchmarks concurrent reads of the same agent,
// which exercises the RWMutex read-lock contention path.
func BenchmarkGetAgentParallel(b *testing.B) {
	agentCounts := []int{10, 100, 1000}

	for _, n := range agentCounts {
		b.Run(fmt.Sprintf("agents=%d", n), func(b *testing.B) {
			store := newBenchStore(b)
			seedAgents(b, store, n)

			targetID := fmt.Sprintf("agent-%04d", n/2)

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if store.GetAgent(targetID) == nil {
						b.Errorf("GetAgent returned nil for %s", targetID)
					}
				}
			})
		})
	}
}

// BenchmarkAllAgents benchmarks returning the full sorted agent list.
// This is a common hot path called by the scheduler and reconciler.
func BenchmarkAllAgents(b *testing.B) {
	agentCounts := []int{10, 100, 1000}

	for _, n := range agentCounts {
		b.Run(fmt.Sprintf("agents=%d", n), func(b *testing.B) {
			store := newBenchStore(b)
			seedAgents(b, store, n)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				agents := store.AllAgents()
				if len(agents) != n {
					b.Fatalf("expected %d agents, got %d", n, len(agents))
				}
			}
		})
	}
}

// BenchmarkSetAgent benchmarks the write path for inserting/updating an agent.
// Each iteration writes a fresh agent with a unique ID to avoid state-transition
// validation errors (agents transition from PENDING only on the first write).
func BenchmarkSetAgent(b *testing.B) {
	agentCounts := []int{10, 100, 1000}

	for _, n := range agentCounts {
		b.Run(fmt.Sprintf("preloaded=%d", n), func(b *testing.B) {
			store := newBenchStore(b)
			seedAgents(b, store, n)

			// Pre-generate unique IDs so ID construction is outside the timed region.
			ids := make([]string, b.N)
			for i := range ids {
				ids[i] = fmt.Sprintf("new-agent-%08d", i)
			}

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := store.SetAgent(&AgentState{
					ID:             ids[i],
					Team:           "bench-team",
					Status:         AgentStatusPending,
					LastTransition: time.Now(),
				}); err != nil {
					b.Fatalf("SetAgent: %v", err)
				}
			}
		})
	}
}

// BenchmarkModifyAgent benchmarks the atomic read-modify-write path.
// This exercises the full ModifyAgent critical section: copy, callback,
// transition validation, DB upsert, and in-memory update.
func BenchmarkModifyAgent(b *testing.B) {
	agentCounts := []int{10, 100, 1000}

	for _, n := range agentCounts {
		b.Run(fmt.Sprintf("agents=%d", n), func(b *testing.B) {
			store := newBenchStore(b)
			seedAgents(b, store, n)

			// Target the middle agent; modify a non-status field to avoid
			// state-machine rejections on repeated iterations.
			targetID := fmt.Sprintf("agent-%04d", n/2)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := store.ModifyAgent(targetID, func(a *AgentState) error {
					// Flip MemoryBytes to simulate a lightweight metadata update.
					a.MemoryBytes = int64(i+1) * 1024 * 1024
					return nil
				}); err != nil {
					b.Fatalf("ModifyAgent: %v", err)
				}
			}
		})
	}
}

// BenchmarkConcurrentReadWrite benchmarks mixed concurrent reads (GetAgent,
// AllAgents) and writes (ModifyAgent) to measure lock contention under a
// realistic workload pattern. The ratio is 8 readers per 1 writer.
func BenchmarkConcurrentReadWrite(b *testing.B) {
	agentCounts := []int{10, 100, 1000}

	for _, n := range agentCounts {
		b.Run(fmt.Sprintf("agents=%d", n), func(b *testing.B) {
			store := newBenchStore(b)
			seedAgents(b, store, n)

			targetID := fmt.Sprintf("agent-%04d", n/2)

			// Use a WaitGroup to coordinate goroutines launched outside RunParallel.
			var wg sync.WaitGroup
			stop := make(chan struct{})

			// Launch background writer.
			wg.Add(1)
			go func() {
				defer wg.Done()
				counter := int64(0)
				for {
					select {
					case <-stop:
						return
					default:
						counter++
						_ = store.ModifyAgent(targetID, func(a *AgentState) error {
							a.MemoryBytes = counter * 1024 * 1024
							return nil
						})
					}
				}
			}()

			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				i := 0
				for pb.Next() {
					if i%2 == 0 {
						store.GetAgent(targetID)
					} else {
						store.AllAgents()
					}
					i++
				}
			})

			close(stop)
			wg.Wait()
		})
	}
}
