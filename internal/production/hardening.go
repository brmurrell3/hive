// Package production provides production hardening utilities for the Hive
// control plane: graceful shutdown, crash recovery, rate limiting, and
// resource monitoring.
package production

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
)

// StoreAccess is the interface for reading and writing agent/node state.
type StoreAccess interface {
	AllAgents() []*state.AgentState
	GetAgent(id string) *state.AgentState
	SetAgent(agent *state.AgentState) error
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
func NewGracefulShutdown(cfg ShutdownConfig) *GracefulShutdown {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
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
	}
}

// Execute performs a graceful shutdown by stopping all running agents and
// waiting for them to exit cleanly within the configured timeout.
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

			g.logger.Info("stopping agent", "agent_id", a.ID)

			if err := g.vmManager.StopVM(ctx, a.ID); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("stopping agent %s: %w", a.ID, err))
				mu.Unlock()

				// Mark as FAILED since we could not stop gracefully.
				a.Status = state.AgentStatusFailed
				a.Error = fmt.Sprintf("graceful shutdown failed: %v", err)
			} else {
				a.Status = state.AgentStatusStopped
				a.Error = ""
			}

			a.LastTransition = time.Now()
			if err := g.store.SetAgent(a); err != nil {
				g.logger.Error("failed to update agent state during shutdown",
					"agent_id", a.ID,
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
		g.logger.Warn("shutdown timed out, some agents may not have stopped cleanly")
		return fmt.Errorf("graceful shutdown timed out: %w", ctx.Err())
	}

	if len(errs) > 0 {
		return fmt.Errorf("shutdown completed with %d errors: %v", len(errs), errs[0])
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
func NewCrashRecovery(cfg RecoveryConfig) *CrashRecovery {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &CrashRecovery{
		store:  cfg.Store,
		logger: logger,
	}
}

// Reconcile checks all agents that claim to be running or starting and
// verifies their processes are actually alive. Dead processes are marked
// as FAILED.
func (cr *CrashRecovery) Reconcile() error {
	agents := cr.store.AllAgents()
	recovered := 0

	for _, agent := range agents {
		if agent.Status != state.AgentStatusRunning && agent.Status != state.AgentStatusStarting {
			continue
		}

		if agent.VMPID <= 0 {
			cr.logger.Warn("agent in active state with no PID, marking as FAILED",
				"agent_id", agent.ID,
				"status", agent.Status,
			)
			agent.Status = state.AgentStatusFailed
			agent.Error = "process not found after crash recovery"
			agent.LastTransition = time.Now()
			if err := cr.store.SetAgent(agent); err != nil {
				cr.logger.Error("failed to update agent state",
					"agent_id", agent.ID,
					"error", err,
				)
			}
			recovered++
			continue
		}

		if !processExists(agent.VMPID) {
			cr.logger.Warn("agent process not found, marking as FAILED",
				"agent_id", agent.ID,
				"pid", agent.VMPID,
				"status", agent.Status,
			)
			agent.Status = state.AgentStatusFailed
			agent.Error = "process not found after crash recovery"
			agent.VMPID = 0
			agent.LastTransition = time.Now()
			if err := cr.store.SetAgent(agent); err != nil {
				cr.logger.Error("failed to update agent state",
					"agent_id", agent.ID,
					"error", err,
				)
			}
			recovered++
		}
	}

	cr.logger.Info("crash recovery complete",
		"agents_checked", len(agents),
		"agents_recovered", recovered,
	)

	return nil
}

// --- Rate Limiter ---

// RateLimiterConfig configures the per-subject rate limiter.
type RateLimiterConfig struct {
	DefaultRate int          // messages per second per subject
	BurstSize   int          // burst allowance
	Logger      *slog.Logger
}

// tokenBucket implements the token bucket algorithm for a single subject.
type tokenBucket struct {
	tokens    float64
	maxTokens float64
	rate      float64 // tokens per second
	lastFill  time.Time
}

// RateLimiter implements per-subject rate limiting using the token bucket
// algorithm.
type RateLimiter struct {
	mu          sync.Mutex
	buckets     map[string]*tokenBucket
	defaultRate int
	burstSize   int
	logger      *slog.Logger
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

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &RateLimiter{
		buckets:     make(map[string]*tokenBucket),
		defaultRate: defaultRate,
		burstSize:   burstSize,
		logger:      logger,
	}
}

// Allow checks whether a message on the given subject is allowed by the rate
// limiter. It returns true if the message is allowed, false if rate-limited.
func (rl *RateLimiter) Allow(subject string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	bucket, ok := rl.buckets[subject]
	if !ok {
		bucket = &tokenBucket{
			tokens:    float64(rl.burstSize),
			maxTokens: float64(rl.burstSize),
			rate:      float64(rl.defaultRate),
			lastFill:  time.Now(),
		}
		rl.buckets[subject] = bucket
	}

	// Refill tokens based on time elapsed.
	now := time.Now()
	elapsed := now.Sub(bucket.lastFill).Seconds()
	bucket.tokens += elapsed * bucket.rate
	if bucket.tokens > bucket.maxTokens {
		bucket.tokens = bucket.maxTokens
	}
	bucket.lastFill = now

	if bucket.tokens < 1 {
		return false
	}

	bucket.tokens--
	return true
}

// SetRate sets a custom rate (messages per second) for a specific subject.
func (rl *RateLimiter) SetRate(subject string, rate int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	bucket, ok := rl.buckets[subject]
	if !ok {
		bucket = &tokenBucket{
			tokens:    float64(rl.burstSize),
			maxTokens: float64(rl.burstSize),
			lastFill:  time.Now(),
		}
		rl.buckets[subject] = bucket
	}

	bucket.rate = float64(rate)

	rl.logger.Info("rate updated for subject",
		"subject", subject,
		"rate", rate,
	)
}

// --- Resource Monitor ---

// MonitorConfig configures the ResourceMonitor.
type MonitorConfig struct {
	Store           StoreAccess
	Metrics         MetricsAccess
	Logger          *slog.Logger
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
	checkInterval   time.Duration
	memoryThreshold float64
	cpuThreshold    float64

	stopOnce sync.Once
	done     chan struct{}
}

// NewResourceMonitor creates a new ResourceMonitor.
func NewResourceMonitor(cfg MonitorConfig) *ResourceMonitor {
	interval := cfg.CheckInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}

	memThreshold := cfg.MemoryThreshold
	if memThreshold <= 0 {
		memThreshold = 0.8
	}

	cpuThreshold := cfg.CPUThreshold
	if cpuThreshold <= 0 {
		cpuThreshold = 0.8
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &ResourceMonitor{
		store:           cfg.Store,
		metrics:         cfg.Metrics,
		logger:          logger,
		checkInterval:   interval,
		memoryThreshold: memThreshold,
		cpuThreshold:    cpuThreshold,
		done:            make(chan struct{}),
	}
}

// Start begins periodic resource monitoring in a background goroutine.
func (rm *ResourceMonitor) Start() error {
	rm.logger.Info("resource monitor started",
		"interval", rm.checkInterval,
		"memory_threshold", rm.memoryThreshold,
		"cpu_threshold", rm.cpuThreshold,
	)

	go rm.loop()
	return nil
}

// Stop halts the resource monitoring loop.
func (rm *ResourceMonitor) Stop() {
	rm.stopOnce.Do(func() {
		close(rm.done)
		rm.logger.Info("resource monitor stopped")
	})
}

// loop is the main monitoring loop that runs in a background goroutine.
func (rm *ResourceMonitor) loop() {
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
// This is a placeholder that returns values based on the node's agent count
// relative to its total resources. In production, this would use actual
// runtime metrics reported by the node.
func (rm *ResourceMonitor) computeUsage(node *types.NodeState) (memPercent, cpuPercent float64) {
	// If the node has no resources reported, return zero.
	if node.Resources.MemoryTotal <= 0 {
		return 0, 0
	}

	// Estimate memory usage based on the number of agents running on this
	// node. Each agent VM uses approximately 512MB by default. This is a
	// rough estimate; real monitoring would use actual process stats.
	agentCount := len(node.Agents)
	memUsedEstimate := int64(agentCount) * 512 * 1024 * 1024
	memPercent = float64(memUsedEstimate) / float64(node.Resources.MemoryTotal) * 100.0
	if memPercent > 100 {
		memPercent = 100
	}

	// Estimate CPU usage based on agent count vs CPU count.
	if node.Resources.CPUCount > 0 {
		cpuPercent = float64(agentCount) / float64(node.Resources.CPUCount) * 25.0 // rough 25% per agent
		if cpuPercent > 100 {
			cpuPercent = 100
		}
	}

	return memPercent, cpuPercent
}
