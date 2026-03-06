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

	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"golang.org/x/time/rate"
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
	DefaultRate int // messages per second per subject
	BurstSize   int // burst allowance
	Logger      *slog.Logger
}

// RateLimiter implements per-subject rate limiting using golang.org/x/time/rate.
type RateLimiter struct {
	mu          sync.Mutex
	limiters    map[string]*rate.Limiter
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
		limiters:    make(map[string]*rate.Limiter),
		defaultRate: defaultRate,
		burstSize:   burstSize,
		logger:      logger,
	}
}

// Allow checks whether a message on the given subject is allowed by the rate
// limiter. It returns true if the message is allowed, false if rate-limited.
func (rl *RateLimiter) Allow(subject string) bool {
	rl.mu.Lock()
	limiter, ok := rl.limiters[subject]
	if !ok {
		limiter = rate.NewLimiter(rate.Limit(rl.defaultRate), rl.burstSize)
		rl.limiters[subject] = limiter
	}
	rl.mu.Unlock()

	return limiter.Allow()
}

// SetRate sets a custom rate (messages per second) for a specific subject.
func (rl *RateLimiter) SetRate(subject string, r int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	limiter, ok := rl.limiters[subject]
	if !ok {
		limiter = rate.NewLimiter(rate.Limit(r), rl.burstSize)
		rl.limiters[subject] = limiter
	} else {
		limiter.SetLimit(rate.Limit(r))
	}

	rl.logger.Info("rate updated for subject",
		"subject", subject,
		"rate", r,
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
// For the local node (detected by matching CPU count), it reads actual system
// resource data. For remote nodes, it falls back to the node's reported
// resource data if available, or returns zero.
func (rm *ResourceMonitor) computeUsage(node *types.NodeState) (memPercent, cpuPercent float64) {
	// If the node has no resources reported, return zero.
	if node.Resources.MemoryTotal <= 0 {
		return 0, 0
	}

	// Determine if this is the local node by comparing CPU count to the
	// system's CPU count. This is a heuristic; in multi-node setups, nodes
	// report their own metrics via heartbeats.
	isLocal := node.Resources.CPUCount == systemCPUCount()

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
