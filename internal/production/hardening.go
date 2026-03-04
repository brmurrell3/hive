// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

// Package production provides production hardening utilities for the Hive
// control plane: graceful shutdown, crash recovery, rate limiting, and
// resource monitoring.
package production

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"golang.org/x/time/rate"
)

const (
	// defaultShutdownTimeout is the default maximum time for graceful shutdown.
	defaultShutdownTimeout = 30 * time.Second

	// defaultResourceCheckInterval is the default interval between resource usage checks.
	defaultResourceCheckInterval = 30 * time.Second

	// rateLimiterCleanupInterval is how often stale rate limiter entries are evicted.
	rateLimiterCleanupInterval = 5 * time.Minute

	// rateLimiterStaleCutoff is how long a rate limiter entry must be idle
	// before it is eligible for eviction.
	rateLimiterStaleCutoff = 10 * time.Minute
)

// StoreAccess is the interface for reading and writing agent/node state.
type StoreAccess interface {
	AllAgents() []*state.AgentState
	GetAgent(id string) *state.AgentState
	SetAgent(agent *state.AgentState) error
	ModifyAgent(id string, fn func(*state.AgentState) error) error
	AllNodes() []*types.NodeState
}

// VMAccess is the interface for stopping VMs.
type VMAccess interface {
	StopVM(ctx context.Context, agentID string) error
}

// MetricsAccess is the interface for recording resource usage metrics.
type MetricsAccess interface {
	SetNodeResourceUsage(nodeID string, memoryPercent, cpuPercent float64)
}

// --- Graceful Shutdown ---

// ShutdownConfig configures the GracefulShutdown handler.
type ShutdownConfig struct {
	Store     StoreAccess
	VMManager VMAccess
	Logger    *slog.Logger
	Timeout   time.Duration // max time to wait for graceful shutdown
}

// GracefulShutdown orchestrates clean shutdown of all hived components.
type GracefulShutdown struct {
	store     StoreAccess
	vmManager VMAccess
	logger    *slog.Logger
	timeout   time.Duration
}

// NewGracefulShutdown creates a new GracefulShutdown handler.
func NewGracefulShutdown(cfg ShutdownConfig) (*GracefulShutdown, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("store is required")
	}
	if cfg.VMManager == nil {
		return nil, fmt.Errorf("vm manager is required")
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultShutdownTimeout
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &GracefulShutdown{
		store:     cfg.Store,
		vmManager: cfg.VMManager,
		logger:    logger,
		timeout:   timeout,
	}, nil
}

// Execute performs a graceful shutdown by stopping all running agents and
// waiting for them to exit cleanly within the configured timeout. When the
// timeout fires, the context passed to goroutines is cancelled so they can
// abort, and we wait for them to finish before returning.
func (g *GracefulShutdown) Execute(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, g.timeout)
	defer cancel()

	g.logger.Info("initiating graceful shutdown", "timeout", g.timeout)

	agents := g.store.AllAgents()

	var running []*state.AgentState
	for _, agent := range agents {
		if agent.Status == state.AgentStatusRunning || agent.Status == state.AgentStatusStarting {
			running = append(running, agent)
		}
	}

	if len(running) == 0 {
		g.logger.Info("no running agents to stop")
		return nil
	}

	g.logger.Info("stopping running agents", "count", len(running))

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	for _, agent := range running {
		wg.Add(1)
		go func(a *state.AgentState) {
			defer wg.Done()

			agentID := a.ID
			g.logger.Info("stopping agent", "agent_id", agentID)

			// StopVM is idempotent for already-stopped VMs.
			stopErr := g.vmManager.StopVM(ctx, agentID)
			if stopErr != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("stopping agent %s: %w", agentID, stopErr))
				mu.Unlock()
			}

			// Use ModifyAgent for atomic state transition to avoid
			// read-copy-modify-SetAgent TOCTOU race.
			if err := g.store.ModifyAgent(agentID, func(ag *state.AgentState) error {
				if stopErr != nil {
					ag.Status = state.AgentStatusFailed
					ag.Error = fmt.Sprintf("graceful shutdown failed: %v", stopErr)
				} else {
					ag.Status = state.AgentStatusStopped
					ag.Error = ""
				}
				ag.LastTransition = time.Now()
				return nil
			}); err != nil {
				g.logger.Error("failed to update agent state during shutdown",
					"agent_id", agentID,
					"error", err,
				)
			}
		}(agent)
	}

	// Wait for all stop operations to complete or context to expire.
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		g.logger.Info("all agents stopped")
	case <-ctx.Done():
		// Timeout fired. The context cancellation will signal goroutines
		// to abort their StopVM calls. Wait for them to actually finish
		// before returning so we don't leak goroutines.
		g.logger.Warn("shutdown timed out, waiting for goroutines to finish")
		<-done
		return fmt.Errorf("graceful shutdown timed out after %s", g.timeout)
	}

	// Safe: wg.Wait() guarantees all goroutines have finished writing to errs.
	if len(errs) > 0 {
		return errors.Join(errs...)
	}

	return nil
}

// --- Crash Recovery ---

// RecoveryConfig configures the CrashRecovery handler.
type RecoveryConfig struct {
	Store  StoreAccess
	Logger *slog.Logger
}

// CrashRecovery detects orphaned processes after an unclean shutdown.
type CrashRecovery struct {
	store  StoreAccess
	logger *slog.Logger
}

// NewCrashRecovery creates a new CrashRecovery handler.
func NewCrashRecovery(cfg RecoveryConfig) (*CrashRecovery, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("store is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &CrashRecovery{
		store:  cfg.Store,
		logger: logger,
	}, nil
}

// Reconcile checks all agents that claim to be running or starting and
// verifies their processes are actually alive. Dead processes are marked
const errProcessNotFound = "process not found after crash recovery"

// as FAILED using ModifyAgent for atomic state transitions.
func (cr *CrashRecovery) Reconcile() error {
	agents := cr.store.AllAgents()
	recovered := 0
	var errs []error

	for _, agent := range agents {
		if agent.Status != state.AgentStatusRunning && agent.Status != state.AgentStatusStarting {
			continue
		}

		agentID := agent.ID

		if agent.VMPID <= 0 {
			cr.logger.Warn("agent in active state with no PID, marking as FAILED",
				"agent_id", agentID,
				"status", agent.Status,
			)
			if err := cr.store.ModifyAgent(agentID, func(ag *state.AgentState) error {
				ag.Status = state.AgentStatusFailed
				ag.Error = errProcessNotFound
				ag.LastTransition = time.Now()
				return nil
			}); err != nil {
				cr.logger.Error("failed to update agent state",
					"agent_id", agentID,
					"error", err,
				)
				errs = append(errs, fmt.Errorf("agent %s: %w", agentID, err))
			}
			recovered++
			continue
		}

		if !processExists(agent.VMPID) {
			cr.logger.Warn("agent process not found, marking as FAILED",
				"agent_id", agentID,
				"pid", agent.VMPID,
				"status", agent.Status,
			)
			if err := cr.store.ModifyAgent(agentID, func(ag *state.AgentState) error {
				ag.Status = state.AgentStatusFailed
				ag.Error = errProcessNotFound
				ag.VMPID = 0
				ag.LastTransition = time.Now()
				return nil
			}); err != nil {
				cr.logger.Error("failed to update agent state",
					"agent_id", agentID,
					"error", err,
				)
				errs = append(errs, fmt.Errorf("agent %s: %w", agentID, err))
			}
			recovered++
		}
	}

	cr.logger.Info("crash recovery complete",
		"agents_checked", len(agents),
		"agents_recovered", recovered,
	)

	if len(errs) > 0 {
		return fmt.Errorf("reconcile had %d errors: %w", len(errs), errors.Join(errs...))
	}
	return nil
}

// --- Rate Limiter ---

// defaultMaxLimiters is the maximum number of unique subjects tracked before
// forcing an immediate cleanup and rejecting new entries if still over limit.
const defaultMaxLimiters = 10000

// RateLimiterConfig configures the per-subject rate limiter.
type RateLimiterConfig struct {
	DefaultRate int // messages per second per subject
	BurstSize   int // burst allowance
	MaxLimiters int // max number of unique subjects; default 10000
	Logger      *slog.Logger
}

// RateLimiter implements per-subject rate limiting using golang.org/x/time/rate.
type RateLimiter struct {
	mu          sync.Mutex
	limiters    map[string]*rateLimiterEntry
	defaultRate int
	burstSize   int
	maxLimiters int
	logger      *slog.Logger
	wg          sync.WaitGroup // tracks background cleanup goroutine
}

// rateLimiterEntry pairs a rate limiter with the last time it was accessed,
// enabling eviction of stale entries to prevent unbounded memory growth.
type rateLimiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

// NewRateLimiter creates a new per-subject rate limiter.
func NewRateLimiter(cfg RateLimiterConfig) *RateLimiter {
	defaultRate := cfg.DefaultRate
	if defaultRate <= 0 {
		defaultRate = 100
	}

	burstSize := cfg.BurstSize
	if burstSize <= 0 {
		burstSize = defaultRate
	}

	maxLimiters := cfg.MaxLimiters
	if maxLimiters <= 0 {
		maxLimiters = defaultMaxLimiters
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &RateLimiter{
		limiters:    make(map[string]*rateLimiterEntry),
		defaultRate: defaultRate,
		burstSize:   burstSize,
		maxLimiters: maxLimiters,
		logger:      logger,
	}
}

// Allow checks whether a message on the given subject is allowed by the rate
// limiter. It returns true if the message is allowed, false if rate-limited.
// If the number of tracked subjects exceeds the configured maximum, an
// immediate cleanup is forced. If the map is still over the threshold after
// cleanup, new (unknown) subjects are rejected to prevent unbounded growth.
func (rl *RateLimiter) Allow(subject string) bool {
	rl.mu.Lock()
	entry, ok := rl.limiters[subject]
	if !ok {
		// Check if we have exceeded the maximum number of tracked subjects.
		if len(rl.limiters) >= rl.maxLimiters {
			// Force an immediate cleanup while holding the lock.
			rl.cleanupLocked()

			// If still over the threshold after cleanup, reject the new entry.
			if len(rl.limiters) >= rl.maxLimiters {
				currentSize := len(rl.limiters)
				rl.mu.Unlock()
				rl.logger.Warn("rate limiter map at capacity, rejecting new subject",
					"subject", subject,
					"size", currentSize,
					"max", rl.maxLimiters,
				)
				return false
			}
		}
		entry = &rateLimiterEntry{
			limiter:  rate.NewLimiter(rate.Limit(rl.defaultRate), rl.burstSize),
			lastSeen: time.Now(),
		}
		rl.limiters[subject] = entry
	} else {
		entry.lastSeen = time.Now()
	}
	allowed := entry.limiter.Allow()
	rl.mu.Unlock()

	return allowed
}

// SetRate sets a custom rate (messages per second) for a specific subject.
// If the subject is new and the limiter map is at capacity, stale entries
// are evicted first. If still at capacity after cleanup, the call is a no-op.
func (rl *RateLimiter) SetRate(subject string, r int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	entry, ok := rl.limiters[subject]
	if !ok {
		// Check capacity before adding a new entry.
		if len(rl.limiters) >= rl.maxLimiters {
			rl.cleanupLocked()
			if len(rl.limiters) >= rl.maxLimiters {
				rl.logger.Warn("rate limiter map at capacity, cannot set rate for new subject",
					"subject", subject,
					"size", len(rl.limiters),
					"max", rl.maxLimiters,
				)
				return
			}
		}
		rl.limiters[subject] = &rateLimiterEntry{
			limiter:  rate.NewLimiter(rate.Limit(r), rl.burstSize),
			lastSeen: time.Now(),
		}
	} else {
		entry.limiter.SetLimit(rate.Limit(r))
		entry.lastSeen = time.Now()
	}

	rl.logger.Info("rate updated for subject",
		"subject", subject,
		"rate", r,
	)
}

// cleanup evicts rate limiter entries that have not been used for over 10 minutes.
func (rl *RateLimiter) cleanup() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.cleanupLocked()
}

// cleanupLocked evicts stale entries. The caller must hold rl.mu.
func (rl *RateLimiter) cleanupLocked() {
	cutoff := time.Now().Add(-rateLimiterStaleCutoff)
	for key, entry := range rl.limiters {
		if entry.lastSeen.Before(cutoff) {
			delete(rl.limiters, key)
		}
	}
}

// StartCleanup begins a background goroutine that periodically evicts stale
// rate limiter entries to prevent unbounded memory growth. The goroutine stops
// when the provided context is cancelled. Use WaitCleanup to wait for the
// goroutine to exit after context cancellation.
func (rl *RateLimiter) StartCleanup(ctx context.Context) {
	rl.wg.Add(1)
	go func() {
		defer rl.wg.Done()
		ticker := time.NewTicker(rateLimiterCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				rl.cleanup()
			case <-ctx.Done():
				return
			}
		}
	}()
}

// WaitCleanup waits for the background cleanup goroutine started by
// StartCleanup to exit. This should be called after the context passed
// to StartCleanup is cancelled.
func (rl *RateLimiter) WaitCleanup() {
	rl.wg.Wait()
}

// --- Resource Monitor ---

// MonitorConfig configures the ResourceMonitor.
type MonitorConfig struct {
	Store           StoreAccess
	Metrics         MetricsAccess
	Logger          *slog.Logger
	LocalNodeID     string // ID of the local node for accurate resource readings
	CheckInterval   time.Duration
	MemoryThreshold float64 // default 0.8 (80%)
	CPUThreshold    float64 // default 0.8
}

// ResourceMonitor periodically checks node resource usage and emits alerts
// when thresholds are exceeded.
type ResourceMonitor struct {
	store           StoreAccess
	metrics         MetricsAccess
	logger          *slog.Logger
	localNodeID     string
	checkInterval   time.Duration
	memoryThreshold float64
	cpuThreshold    float64

	mu       sync.Mutex
	started  bool
	stopOnce sync.Once
	done     chan struct{}
	wg       sync.WaitGroup
}

// NewResourceMonitor creates a new ResourceMonitor. The LocalNodeID field in
// the config identifies which node in the store represents the local machine,
// enabling accurate resource readings from the OS rather than estimates.
func NewResourceMonitor(cfg MonitorConfig) (*ResourceMonitor, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("store is required")
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	interval := cfg.CheckInterval
	if interval <= 0 {
		interval = defaultResourceCheckInterval
	}

	memThreshold := cfg.MemoryThreshold
	if memThreshold <= 0 {
		memThreshold = 0.8
	}
	// Clamp to [0.01, 1.0] to prevent nonsensical threshold values.
	if memThreshold < 0.01 {
		logger.Warn("memory threshold clamped", "original", cfg.MemoryThreshold, "clamped", 0.01)
		memThreshold = 0.01
	}
	if memThreshold > 1.0 {
		logger.Warn("memory threshold clamped", "original", cfg.MemoryThreshold, "clamped", 1.0)
		memThreshold = 1.0
	}

	cpuThreshold := cfg.CPUThreshold
	if cpuThreshold <= 0 {
		cpuThreshold = 0.8
	}
	// Clamp to [0.01, 1.0] to prevent nonsensical threshold values.
	if cpuThreshold < 0.01 {
		logger.Warn("CPU threshold clamped", "original", cfg.CPUThreshold, "clamped", 0.01)
		cpuThreshold = 0.01
	}
	if cpuThreshold > 1.0 {
		logger.Warn("CPU threshold clamped", "original", cfg.CPUThreshold, "clamped", 1.0)
		cpuThreshold = 1.0
	}

	return &ResourceMonitor{
		store:           cfg.Store,
		metrics:         cfg.Metrics,
		logger:          logger,
		localNodeID:     cfg.LocalNodeID,
		checkInterval:   interval,
		memoryThreshold: memThreshold,
		cpuThreshold:    cpuThreshold,
		done:            make(chan struct{}),
	}, nil
}

// Start begins periodic resource monitoring in a background goroutine.
// It is safe to call only once; subsequent calls return an error.
func (rm *ResourceMonitor) Start() error {
	rm.mu.Lock()
	if rm.started {
		rm.mu.Unlock()
		return fmt.Errorf("resource monitor already started")
	}
	rm.started = true
	rm.mu.Unlock()

	rm.logger.Info("resource monitor started",
		"interval", rm.checkInterval,
		"memory_threshold", rm.memoryThreshold,
		"cpu_threshold", rm.cpuThreshold,
	)

	rm.wg.Add(1)
	go rm.loop()
	return nil
}

// Stop halts the resource monitoring loop and waits for the goroutine to exit.
// The "stopped" log message is emitted after the goroutine has fully exited.
// If Start() was never called, Stop() is a no-op.
func (rm *ResourceMonitor) Stop() {
	rm.mu.Lock()
	wasStarted := rm.started
	rm.mu.Unlock()

	rm.stopOnce.Do(func() {
		close(rm.done)
	})
	rm.wg.Wait()
	if wasStarted {
		rm.logger.Info("resource monitor stopped")
	}
}

// loop is the main monitoring loop that runs in a background goroutine.
func (rm *ResourceMonitor) loop() {
	defer rm.wg.Done()

	ticker := time.NewTicker(rm.checkInterval)
	defer ticker.Stop()

	for {
		select {
		case <-rm.done:
			return
		case <-ticker.C:
			rm.check()
		}
	}
}

// check performs a single resource usage check across all nodes.
func (rm *ResourceMonitor) check() {
	nodes := rm.store.AllNodes()

	for _, node := range nodes {
		if node.Status != types.NodeStatusOnline {
			continue
		}

		// Compute usage percentages from the node's resource data.
		// In a real deployment, resource usage would be reported via heartbeats.
		// Here we report what we know and check thresholds.
		memPercent, cpuPercent := rm.computeUsage(node)

		// Record to metrics.
		if rm.metrics != nil {
			rm.metrics.SetNodeResourceUsage(node.ID, memPercent, cpuPercent)
		}

		// Check thresholds and log warnings.
		if memPercent/100.0 > rm.memoryThreshold {
			rm.logger.Warn("node memory usage exceeds threshold",
				"node_id", node.ID,
				"memory_percent", memPercent,
				"threshold_percent", rm.memoryThreshold*100,
			)
		}

		if cpuPercent/100.0 > rm.cpuThreshold {
			rm.logger.Warn("node CPU usage exceeds threshold",
				"node_id", node.ID,
				"cpu_percent", cpuPercent,
				"threshold_percent", rm.cpuThreshold*100,
			)
		}
	}
}

// computeUsage calculates memory and CPU usage percentages for a node.
// For the local node (identified by matching the configured LocalNodeID), it
// reads actual system resource data. For remote nodes, it falls back to the
// node's reported resource data if available, or returns zero.
func (rm *ResourceMonitor) computeUsage(node *types.NodeState) (memPercent, cpuPercent float64) {
	// If the node has no resources reported, return zero.
	if node.Resources.MemoryTotal <= 0 {
		return 0, 0
	}

	// Determine if this is the local node by comparing node ID to the
	// configured local node ID. This is reliable across multi-node setups
	// where multiple nodes may have the same CPU count.
	isLocal := rm.localNodeID != "" && node.ID == rm.localNodeID

	if isLocal {
		memPercent, cpuPercent = rm.computeLocalUsage(node)
	} else {
		memPercent, cpuPercent = rm.computeRemoteUsage(node)
	}

	return memPercent, cpuPercent
}

// computeLocalUsage reads actual system resource data from the host OS.
func (rm *ResourceMonitor) computeLocalUsage(node *types.NodeState) (memPercent, cpuPercent float64) {
	// Read actual memory usage from the OS.
	memPct, _, _, err := systemMemoryUsage()
	if err != nil {
		rm.logger.Warn("failed to read system memory usage, falling back to estimate",
			"node_id", node.ID,
			"error", err,
		)
		memPercent = rm.estimateMemoryFromAgents(node)
	} else {
		memPercent = memPct
	}

	// Read actual CPU usage from the OS.
	cpuPct, err := systemCPUUsage()
	if err != nil {
		rm.logger.Warn("failed to read system CPU usage, falling back to estimate",
			"node_id", node.ID,
			"error", err,
		)
		cpuPercent = rm.estimateCPUFromAgents(node)
	} else {
		cpuPercent = cpuPct
	}

	return memPercent, cpuPercent
}

// computeRemoteUsage estimates resource usage for a remote node based on
// its agent count. In a full deployment, remote nodes report actual usage
// via heartbeats. This provides a fallback estimate when real data is
// unavailable.
func (rm *ResourceMonitor) computeRemoteUsage(node *types.NodeState) (memPercent, cpuPercent float64) {
	memPercent = rm.estimateMemoryFromAgents(node)
	cpuPercent = rm.estimateCPUFromAgents(node)
	return memPercent, cpuPercent
}

// estimateMemoryFromAgents estimates memory usage based on agent count.
// Each agent VM uses approximately 512MB. This is only used as a fallback
// when actual system readings are unavailable.
func (rm *ResourceMonitor) estimateMemoryFromAgents(node *types.NodeState) float64 {
	if node.Resources.MemoryTotal <= 0 {
		return 0
	}
	agentCount := len(node.Agents)
	memUsedEstimate := int64(agentCount) * 512 * 1024 * 1024
	pct := float64(memUsedEstimate) / float64(node.Resources.MemoryTotal) * 100.0
	if pct > 100 {
		pct = 100
	}
	return pct
}

// estimateCPUFromAgents estimates CPU usage based on agent count vs CPU count.
// This is only used as a fallback when actual system readings are unavailable.
func (rm *ResourceMonitor) estimateCPUFromAgents(node *types.NodeState) float64 {
	if node.Resources.CPUCount <= 0 {
		return 0
	}
	agentCount := len(node.Agents)
	pct := float64(agentCount) / float64(node.Resources.CPUCount) * 25.0
	if pct > 100 {
		pct = 100
	}
	return pct
}
