// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/brmurrell3/hive/internal/auth"
	"github.com/brmurrell3/hive/internal/backend"
	fcbackend "github.com/brmurrell3/hive/internal/backend/firecracker"
	procbackend "github.com/brmurrell3/hive/internal/backend/process"
	"github.com/brmurrell3/hive/internal/capability"
	"github.com/brmurrell3/hive/internal/cluster"
	"github.com/brmurrell3/hive/internal/config"
	"github.com/brmurrell3/hive/internal/dashboard"
	"github.com/brmurrell3/hive/internal/events"
	"github.com/brmurrell3/hive/internal/health"
	"github.com/brmurrell3/hive/internal/logging"
	"github.com/brmurrell3/hive/internal/logs"
	"github.com/brmurrell3/hive/internal/metrics"
	hivenats "github.com/brmurrell3/hive/internal/nats"
	"github.com/brmurrell3/hive/internal/node"
	"github.com/brmurrell3/hive/internal/production"
	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/reconciler"
	"github.com/brmurrell3/hive/internal/scheduler"
	"github.com/brmurrell3/hive/internal/secrets"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/brmurrell3/hive/internal/vm"
	"github.com/brmurrell3/hive/internal/watcher"
	"github.com/nats-io/nats.go"
)

var version = "dev"

// controlHandlerConfig holds all dependencies for the control handler,
// replacing the long parameter list in newControlHandler.
type controlHandlerConfig struct {
	RunCtx       context.Context
	ClusterRoot  string
	Store        *state.Store
	AgentMgr     *backend.AgentManager
	Scheduler    *scheduler.Scheduler
	Monitor      *health.Monitor
	Reconciler   *reconciler.Reconciler
	ClusterMgr   *cluster.Cluster
	Authorizer   *auth.Authorizer
	Events       *events.Publisher
	NodeRegistry *node.Registry
	RateLimiter  *production.RateLimiter
	Metrics      *metrics.Collector
	NATSConn     *nats.Conn
	Logger       *slog.Logger
}

// Rate limiter defaults for control plane requests.
// These can be adjusted to tune the rate at which hivectl commands are accepted.
const (
	defaultRateLimitRate  = 100 // requests per second per subject
	defaultRateLimitBurst = 20  // burst allowance above steady-state rate
)

// Resource monitoring thresholds. When memory or CPU utilization exceeds
// these fractions, the resource monitor emits warnings.
const (
	defaultResourceCheckInterval = 30 * time.Second
	defaultMemoryThreshold       = 0.9
	defaultCPUThreshold          = 0.9
)

// Shutdown and startup timing constants.
const (
	// dashboardStartupWait is the time to wait for the dashboard HTTP server
	// to bind its listener on startup. This is a startup race workaround;
	// ideally the dashboard server would expose a ready channel.
	dashboardStartupWait = 2 * time.Second

	// gracefulShutdownTimeout is the maximum time to wait for graceful
	// shutdown of agents and subsystems.
	gracefulShutdownTimeout = 30 * time.Second

	// metricsShutdownTimeout is the maximum time for the metrics HTTP
	// server to drain in-flight requests during shutdown.
	metricsShutdownTimeout = 5 * time.Second

	// dashboardShutdownTimeout is the maximum time for the dashboard
	// HTTP server to drain during shutdown.
	dashboardShutdownTimeout = 10 * time.Second
)

func main() {
	clusterRoot := flag.String("cluster-root", ".", "Path to the cluster root directory")
	showVersion := flag.Bool("version", false, "Print version and exit")
	logLevel := flag.String("log-level", "info", "Log level (debug, info, warn, error)")
	flag.Parse()

	if *showVersion {
		fmt.Printf("hived %s\n", version)
		os.Exit(0)
	}

	level := logging.ParseLevel(*logLevel)
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: level,
	}))
	slog.SetDefault(logger)

	if err := run(*clusterRoot, logger); err != nil {
		logger.Error("hived failed", "error", err)
		os.Exit(1)
	}
}

func run(clusterRoot string, logger *slog.Logger) error {
	// Top-level context cancelled on shutdown; child subsystems should derive
	// from this so they stop when hived shuts down.
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()

	absRoot, err := filepath.Abs(clusterRoot)
	if err != nil {
		return fmt.Errorf("resolving cluster root: %w", err)
	}

	logger.Info("starting hived", "cluster_root", absRoot)

	cfg, err := config.LoadCluster(absRoot)
	if err != nil {
		return fmt.Errorf("loading cluster config: %w", err)
	}

	logger.Info("cluster config loaded",
		"name", cfg.Metadata.Name,
		"nats_port", cfg.Spec.NATS.Port,
		"jetstream", cfg.Spec.NATS.JetStream.IsEnabled(),
	)

	dataDir := filepath.Join(absRoot, ".state", "jetstream")
	if cfg.Spec.NATS.JetStream.StorePath != "" {
		dataDir = cfg.Spec.NATS.JetStream.StorePath
	}

	ns, err := hivenats.NewServer(cfg.Spec.NATS, dataDir, logger)
	if err != nil {
		return fmt.Errorf("creating NATS server: %w", err)
	}
	if err := ns.Start(); err != nil {
		return fmt.Errorf("starting NATS server: %w", err)
	}
	defer ns.Shutdown()

	statePath := filepath.Join(absRoot, "state.db")
	store, err := state.NewStore(statePath, logger)
	if err != nil {
		return fmt.Errorf("initializing state store: %w", err)
	}
	defer store.Close() //nolint:errcheck // best-effort cleanup on deferred close

	// Write the NATS auth token to .state/nats-auth-token so hivectl can read it.
	stateDir := filepath.Join(absRoot, ".state")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	tokenPath := filepath.Join(stateDir, "nats-auth-token")
	if err := os.WriteFile(tokenPath, []byte(ns.AuthToken()), 0600); err != nil {
		return fmt.Errorf("writing NATS auth token: %w", err)
	}
	logger.Debug("NATS auth token written", "path", tokenPath)

	// natsClosedCh is closed by the NATS ClosedHandler to trigger graceful
	// shutdown without resorting to sending SIGTERM to self. The main loop
	// selects on both this channel and the OS signal context.
	natsClosedCh := make(chan struct{})

	// Connect to the embedded NATS server as a client using the auth token.
	nc, err := nats.Connect(ns.ClientURL(),
		nats.Token(ns.AuthToken()),
		nats.MaxReconnects(60), // ~2 minutes at 2s reconnect interval
		nats.ReconnectWait(2*time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			logger.Warn("NATS client disconnected", "error", err)
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			logger.Info("NATS client reconnected")
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			logger.Error("NATS connection permanently closed, initiating shutdown", "error", nc.LastError())
			// Signal the main loop to begin graceful shutdown via channel
			// close instead of sending SIGTERM to self, which is fragile
			// and can interact poorly with process supervisors.
			select {
			case <-natsClosedCh:
				// Already closed, avoid double-close panic.
			default:
				close(natsClosedCh)
			}
		}),
	)
	if err != nil {
		return fmt.Errorf("connecting to embedded NATS: %w", err)
	}
	if !nc.IsConnected() {
		nc.Close()
		return fmt.Errorf("NATS connection is not in connected state after Connect()")
	}
	defer func() {
		if err := nc.Drain(); err != nil {
			logger.Error("NATS drain failed", "error", err)
		}
	}()

	eventPub := events.NewPublisher(nc, "hived", logger)
	logger.Info("event publisher initialized")

	// No dependencies -- safe to initialize early.
	collector := metrics.NewCollector()
	logger.Info("metrics collector initialized")

	logDir := filepath.Join(absRoot, ".state", "logs")
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}
	logAgg := logs.NewAggregator(logs.AggregatorConfig{
		NATSConn:      nc,
		LogDir:        logDir,
		RetentionDays: cfg.Spec.Logging.EffectiveRetentionDays(),
		Logger:        logger,
	})
	if cfg.Spec.Logging.Enabled {
		if err := logAgg.Start(); err != nil {
			return fmt.Errorf("starting log aggregator: %w", err)
		}
		defer logAgg.Stop()
		logger.Info("log aggregator started", "log_dir", logDir)
	}

	// Determine hypervisor: use mock if explicitly requested, if not on Linux,
	// if /dev/kvm is not available, or if firecracker is not installed.
	var hyp vm.Hypervisor
	useMock := os.Getenv("HIVE_TEST_FIRECRACKER") == "mock"
	if !useMock {
		if runtime.GOOS != "linux" {
			logger.Warn("not running on Linux — Firecracker requires KVM, falling back to mock hypervisor",
				"os", runtime.GOOS)
			useMock = true
		} else if _, err := os.Stat("/dev/kvm"); err != nil {
			logger.Warn("/dev/kvm not available — Firecracker requires KVM, falling back to mock hypervisor",
				"error", err)
			useMock = true
		} else if _, err := exec.LookPath("firecracker"); err != nil {
			logger.Warn("firecracker binary not found in PATH, falling back to mock hypervisor")
			useMock = true
		}
	}
	if useMock {
		hyp = vm.NewMockHypervisor()
		logger.Info("using mock hypervisor")
	} else {
		fcHyp, err := vm.NewFirecrackerHypervisor("", logger)
		if err != nil {
			return fmt.Errorf("initializing Firecracker hypervisor: %w", err)
		}
		hyp = fcHyp
	}

	// NATS port and auth token are passed to the VM manager for vsock
	// forwarding and sidecar.conf injection.
	natsPort := cfg.Spec.NATS.Port
	if natsPort == 0 {
		natsPort = 4222
	}
	vmMgr := vm.NewManager(absRoot, store, logger, hyp, uint32(natsPort), ns.AuthToken(), cfg.Spec.VM.TotalMemoryMB, cfg.Spec.VM.TotalVCPUs)
	if err := vmMgr.ReconcileOnStartup(); err != nil {
		logger.Error("startup reconciliation failed: VM state may be inconsistent with the state store; "+
			"stale VMs or resource leaks may persist until the next reconciliation cycle",
			"error", err)
	}

	backendRegistry := backend.NewRegistry()
	fcBack := fcbackend.New(vmMgr, store, logger)
	if err := backendRegistry.Register(fcBack); err != nil {
		return fmt.Errorf("registering firecracker backend: %w", err)
	}
	procBack := procbackend.New(logger, store)
	if err := backendRegistry.Register(procBack); err != nil {
		return fmt.Errorf("registering process backend: %w", err)
	}
	agentMgr := backend.NewAgentManager(backendRegistry, "firecracker", logger)

	// Load agent manifests to determine correct backend for each agent.
	startupAgents, startupAgentsErr := config.LoadAgents(absRoot)
	if startupAgentsErr != nil {
		logger.Warn("failed to load agent manifests at startup; backend selection may be incomplete", "error", startupAgentsErr)
	}
	for _, a := range store.AllAgents() {
		if a.Status == state.AgentStatusRunning || a.Status == state.AgentStatusStarting {
			backendName := ""
			if manifest, ok := startupAgents[a.ID]; ok {
				backendName = manifest.Spec.Runtime.Backend
			}
			agentMgr.RegisterAgentFromState(a.ID, backendName)
		}
	}
	logger.Info("agent manager initialized", "backends", backendRegistry.List())

	// Register capabilities from loaded manifests so the capability registry
	// is populated at startup. Without this, cross-team routing, capability
	// discovery, and hivectl capabilities list all see an empty registry.
	if startupAgents != nil {
		for agentID, manifest := range startupAgents {
			if len(manifest.Spec.Capabilities) == 0 {
				continue
			}
			tier := "vm"
			if manifest.Spec.Runtime.Backend == "process" || manifest.Spec.Runtime.Backend == "nspawn" {
				tier = "native"
			}
			if err := store.RegisterCapabilities(agentID, manifest.Metadata.Team, tier, "", manifest.Spec.Capabilities); err != nil {
				logger.Warn("failed to register capabilities from manifest",
					"agent_id", agentID,
					"error", err,
				)
			}
		}
		logger.Info("capabilities registered from manifests", "agent_count", len(startupAgents))
	}

	// Subscribe to runtime capability registration from sidecars.
	// Agents publish here after startup so the registry stays in sync
	// with capabilities that are actually online.
	if _, err := nc.Subscribe(protocol.SubjCapabilityRegister, func(msg *nats.Msg) {
		var regReq struct {
			AgentID      string                  `json:"agent_id"`
			TeamID       string                  `json:"team_id"`
			Tier         string                  `json:"tier"`
			NodeID       string                  `json:"node_id"`
			Capabilities []types.AgentCapability `json:"capabilities"`
		}
		if err := json.Unmarshal(msg.Data, &regReq); err != nil {
			logger.Warn("invalid capability registration message", "error", err)
			return
		}
		if regReq.AgentID == "" {
			return
		}
		if err := store.RegisterCapabilities(regReq.AgentID, regReq.TeamID, regReq.Tier, regReq.NodeID, regReq.Capabilities); err != nil {
			logger.Warn("failed to register capabilities from sidecar",
				"agent_id", regReq.AgentID,
				"error", err,
			)
			return
		}

		// Ensure external agents have an accurate state entry.
		// External agents aren't managed by hived's lifecycle, so if
		// they're registering capabilities they are definitively running.
		existing := store.GetAgent(regReq.AgentID)
		if existing != nil && existing.Status == state.AgentStatusFailed {
			// Remove stale FAILED entry so we can recreate as RUNNING.
			if err := store.RemoveAgent(regReq.AgentID); err != nil {
				logger.Warn("failed to remove stale agent state",
					"agent_id", regReq.AgentID,
					"error", err,
				)
			}
			existing = nil
		}
		if existing == nil {
			now := time.Now().UTC()
			if err := store.SetAgent(&state.AgentState{
				ID:             regReq.AgentID,
				Team:           regReq.TeamID,
				Status:         state.AgentStatusRunning,
				NodeID:         regReq.NodeID,
				LastTransition: now,
				StartedAt:      now,
			}); err != nil {
				logger.Warn("failed to create agent state for external agent",
					"agent_id", regReq.AgentID,
					"error", err,
				)
			}
		}

		logger.Debug("capabilities registered from sidecar",
			"agent_id", regReq.AgentID,
			"count", len(regReq.Capabilities),
		)
	}); err != nil {
		return fmt.Errorf("subscribing to capability registration: %w", err)
	}

	monitor := health.NewMonitor(
		store, nc,
		cfg.Spec.Defaults.Health.Interval,
		cfg.Spec.Defaults.Health.MaxFailures,
		logger,
	)

	restartMgr := health.NewRestartManager(store, agentMgr, logger)
	monitor.SetRestartManager(restartMgr)

	if err := monitor.Start(); err != nil {
		return fmt.Errorf("starting health monitor: %w", err)
	}
	defer monitor.Stop()

	capRouter := capability.NewRouter("hived", nc, logger)
	capRouter.SetEventPublisher(eventPub)
	if err := capRouter.Start(); err != nil {
		return fmt.Errorf("starting capability router: %w", err)
	}
	defer capRouter.Stop()

	secretsPath := filepath.Join(absRoot, ".state", "secrets.yaml")
	if _, err := os.Stat(secretsPath); os.IsNotExist(err) {
		secretsPath = "" // no secrets file, create empty store
	}
	secretStore, err := secrets.NewStore(secretsPath, nc, logger)
	if err != nil {
		return fmt.Errorf("creating secrets store: %w", err)
	}
	if err := secretStore.Start(); err != nil {
		return fmt.Errorf("starting secrets store: %w", err)
	}
	defer secretStore.Stop()
	// Build per-agent secret access list from manifests so the secret
	// store only returns secrets an agent is configured to receive.
	// If manifest loading failed, use an empty map (deny-all) rather than
	// nil (allow-all) to prevent a fail-open security gap.
	{
		allowed := make(map[string]map[string]bool)
		for id, manifest := range startupAgents {
			names := make(map[string]bool, len(manifest.Spec.Secrets))
			for _, s := range manifest.Spec.Secrets {
				names[s.Name] = true
			}
			allowed[id] = names
		}
		secretStore.SetAllowedSecrets(allowed)
	}
	logger.Info("secrets store started")

	// Compute the advertised NATS address for joining nodes.
	// This resolves the configured host + port into a URL reachable from
	// remote machines, handling 0.0.0.0 and empty host by discovering the
	// machine's outbound IP.
	advertiseAddr := node.ResolveAdvertiseAddr(cfg.Spec.NATS.Host, natsPort)
	if advertiseAddr != "" {
		logger.Info("node registry advertise address", "nats_advertise_url", advertiseAddr)
	} else {
		logger.Warn("could not determine advertise address for node registry, " +
			"joining nodes will receive the local connection URL which may be unreachable")
	}

	nodeRegistry := node.NewRegistry(store, nc, logger, advertiseAddr)
	nodeRegistry.SetEventPublisher(eventPub)
	if err := nodeRegistry.Start(); err != nil {
		return fmt.Errorf("starting node registry: %w", err)
	}
	defer nodeRegistry.Stop() //nolint:errcheck // best-effort cleanup on deferred close

	// Load desired state (agents + teams + cluster config with merged defaults)
	// so we can populate RestartManager configs for every agent.
	desiredState, err := config.LoadDesiredState(absRoot)
	if err != nil {
		// Non-fatal: agents directory may not exist yet on a fresh cluster.
		logger.Warn("could not load desired state for restart configs", "error", err)
	} else {
		var configuredCount int
		for agentID, manifest := range desiredState.Agents {
			// Skip external agents — hived cannot restart what it doesn't own.
			if !manifest.Spec.IsManaged() {
				continue
			}
			restartMgr.SetConfig(agentID, health.RestartConfig{
				Policy:      manifest.Spec.Restart.Policy,
				MaxRestarts: manifest.Spec.Restart.MaxRestarts,
				Backoff:     manifest.Spec.Restart.Backoff,
				Manifest:    manifest,
			})
			configuredCount++
		}
		logger.Info("restart configs populated", "agent_count", configuredCount)
	}

	sched := scheduler.NewScheduler(store, logger)
	logger.Info("scheduler initialized")

	// clusterMgrRef is set after the cluster manager is initialized below.
	// The reconciler action handler captures this pointer so it can replicate
	// state changes to other cluster nodes. We use atomic.Pointer to avoid a
	// data race between the write (after cluster manager init) and concurrent
	// reads from reconciler action callbacks.
	var clusterMgrRef atomic.Pointer[cluster.Cluster]

	rec := reconciler.NewReconciler(store, absRoot, logger)
	rec.SetScheduler(&schedulerAdapter{s: sched})
	// replicateViaCluster replicates agent state to other cluster nodes when
	// the cluster manager is available.
	replicateViaCluster := func(agentID string) {
		cm := clusterMgrRef.Load()
		if cm == nil {
			return
		}
		agent := store.GetAgent(agentID)
		if agent == nil {
			return
		}
		data, err := json.Marshal(agent)
		if err != nil {
			logger.Warn("failed to marshal agent state for replication",
				"agent_id", agentID, "error", err)
			return
		}
		if err := cm.ReplicateAgentState(agentID, data); err != nil {
			logger.Warn("failed to replicate agent state", "agent_id", agentID, "error", err)
		}
	}
	rec.SetActionHandler(func(action reconciler.Action) error {
		switch action.Type {
		case reconciler.ActionCreate:
			startCtx, startCancel := context.WithTimeout(runCtx, 2*time.Minute)
			defer startCancel()
			if err := agentMgr.StartAgent(startCtx, action.Manifest); err != nil {
				return err
			}
			eventPub.AgentCreated(action.AgentID, action.Manifest.Metadata.Team)
			replicateViaCluster(action.AgentID)
			return nil
		case reconciler.ActionDestroy:
			destroyCtx, destroyCancel := context.WithTimeout(runCtx, 2*time.Minute)
			defer destroyCancel()
			if err := agentMgr.DestroyAgent(destroyCtx, action.AgentID); err != nil {
				return err
			}
			eventPub.AgentStopped(action.AgentID)
			// Release scheduler allocations so the in-memory resource tracking
			// stays accurate. Log but don't fail the destroy on error.
			if err := sched.ReleaseAgent(action.AgentID); err != nil {
				logger.Warn("scheduler release failed after destroy",
					"agent_id", action.AgentID,
					"error", err,
				)
			}
			// Remove agent from health monitor to prevent unbounded map growth.
			monitor.RemoveAgent(action.AgentID)
			// Clean up stale agent metric labels to prevent unbounded cardinality growth.
			collector.RemoveAgent(action.AgentID)
			replicateViaCluster(action.AgentID)
			return nil
		case reconciler.ActionRestart:
			restartCtx, restartCancel := context.WithTimeout(runCtx, 2*time.Minute)
			defer restartCancel()
			if err := agentMgr.RestartAgent(restartCtx, action.AgentID, action.Manifest); err != nil {
				return err
			}
			replicateViaCluster(action.AgentID)
			return nil
		case reconciler.ActionUpdate:
			// Metadata-only change (labels, team, description). Update the
			// agent state without restarting the runtime.
			if err := store.ModifyAgent(action.AgentID, func(a *state.AgentState) error {
				if action.Manifest != nil {
					a.Team = action.Manifest.Metadata.Team
				}
				return nil
			}); err != nil {
				return err
			}
			replicateViaCluster(action.AgentID)
			return nil
		default:
			logger.Warn("unknown reconciler action", "type", action.Type)
			return nil
		}
	})
	if err := rec.Start(); err != nil {
		return fmt.Errorf("starting reconciler: %w", err)
	}
	defer rec.Stop()

	// Start MEMORY.md file watcher. When an agent's MEMORY.md changes on disk,
	// the watcher reads the new content and publishes it to
	// hive.agent.{agentID}.memory so that the sidecar running inside the VM
	// can receive it and write it into the agent's workspace.
	memWatcher, err := watcher.NewWatcher(absRoot, func(agentID string, content []byte) {
		subject := fmt.Sprintf(protocol.FmtAgentMemory, agentID)
		contentPayload, err := json.Marshal(string(content))
		if err != nil {
			logger.Error("failed to marshal memory update payload",
				"agent_id", agentID,
				"error", err,
			)
			return
		}
		env := types.Envelope{
			ID:        types.NewUUID(),
			From:      "hived",
			To:        agentID,
			Type:      types.MessageTypeMemoryUpdate,
			Timestamp: time.Now().UTC(),
			Payload:   contentPayload,
		}
		data, err := json.Marshal(env)
		if err != nil {
			logger.Error("failed to marshal memory update envelope",
				"agent_id", agentID,
				"error", err,
			)
			return
		}
		if err := nc.Publish(subject, data); err != nil {
			logger.Error("failed to publish memory update",
				"agent_id", agentID,
				"subject", subject,
				"error", err,
			)
			return
		}
		if err := nc.FlushTimeout(2 * time.Second); err != nil {
			logger.Warn("failed to flush memory update",
				"agent_id", agentID,
				"subject", subject,
				"error", err,
			)
		}
		logger.Info("published MEMORY.md update to NATS",
			"agent_id", agentID,
			"subject", subject,
			"size_bytes", len(content),
		)
	}, logger)
	if err != nil {
		return fmt.Errorf("creating memory watcher: %w", err)
	}
	if err := memWatcher.Start(); err != nil {
		return fmt.Errorf("starting memory watcher: %w", err)
	}
	defer memWatcher.Stop() //nolint:errcheck // best-effort cleanup on deferred close

	// Build RBAC authorizer from stored users (if any).
	// When no users are configured, the authorizer is nil and all operations
	// are allowed (backward compatible single-user mode).
	var authorizer *auth.Authorizer
	storedUsers := store.AllUsers()
	if len(storedUsers) > 0 {
		users := make([]auth.User, len(storedUsers))
		for i, u := range storedUsers {
			users[i] = *u
		}
		authorizer = auth.NewAuthorizer(users, logger)
		logger.Info("RBAC authorizer initialized", "user_count", len(users))
	} else {
		logger.Info("no RBAC users configured, authorization checks disabled")
	}

	// Create a rate limiter for control plane requests.
	// Derived from runCtx so the background cleanup goroutine stops when
	// hived shuts down.
	rateLimiterCtx, rateLimiterCancel := context.WithCancel(runCtx)
	defer rateLimiterCancel()
	rateLimiter := production.NewRateLimiter(production.RateLimiterConfig{
		DefaultRate: defaultRateLimitRate,
		BurstSize:   defaultRateLimitBurst,
		Logger:      logger,
	})
	rateLimiter.StartCleanup(rateLimiterCtx)

	// Run crash recovery to reconcile stale state from a previous unclean shutdown.
	crashRecovery, err := production.NewCrashRecovery(production.RecoveryConfig{
		Store:  store,
		Logger: logger,
	})
	if err != nil {
		return fmt.Errorf("creating crash recovery: %w", err)
	}
	if err := crashRecovery.Reconcile(); err != nil {
		logger.Error("crash recovery reconciliation failed: agents marked as running in the state store "+
			"may no longer have live processes; manual inspection via 'hivectl agents' is recommended",
			"error", err)
	}

	resMon, err := production.NewResourceMonitor(production.MonitorConfig{
		Store:           store,
		Metrics:         collector,
		Logger:          logger,
		LocalNodeID:     cfg.Metadata.Name,
		CheckInterval:   defaultResourceCheckInterval,
		MemoryThreshold: defaultMemoryThreshold,
		CPUThreshold:    defaultCPUThreshold,
	})
	if err != nil {
		return fmt.Errorf("creating resource monitor: %w", err)
	}
	if err := resMon.Start(); err != nil {
		return fmt.Errorf("starting resource monitor: %w", err)
	}
	defer resMon.Stop()

	clusterRole := cluster.Role("root")
	var clusterTLS *types.TLSConfig
	if cfg.Spec.NATS.TLS.Enabled {
		clusterTLS = &cfg.Spec.NATS.TLS
	}
	clusterMgr := cluster.NewCluster(cluster.Config{
		Role:      clusterRole,
		NATSMode:  cfg.Spec.NATS.Mode,
		NATSUrls:  cfg.Spec.NATS.URLs,
		TLS:       clusterTLS,
		AuthToken: cfg.Spec.NATS.AuthToken,
	}, store, logger)
	if err := clusterMgr.Start(); err != nil {
		return fmt.Errorf("starting cluster manager: %w", err)
	}
	defer clusterMgr.Stop()

	// Wire the cluster manager into the reconciler action handler so it can
	// replicate state changes to other cluster nodes.
	clusterMgrRef.Store(clusterMgr)

	// Subscribe to cluster state updates (used by worker nodes in external NATS mode).
	if err := clusterMgr.SubscribeStateUpdates(func(data []byte) {
		// Apply replicated agent state from the leader node.
		// Replicated fields: Status, Team.
		// Excluded fields: ID (used as key, not mutated), CID/VMID (local to
		// the node running the VM), CreatedAt (immutable), Config (loaded from
		// manifests on each node). Only convergence-critical fields are
		// replicated to minimize bandwidth and avoid overwriting node-local state.
		var agentState state.AgentState
		if err := json.Unmarshal(data, &agentState); err != nil {
			logger.Warn("failed to unmarshal replicated agent state", "error", err)
			return
		}
		if err := store.ModifyAgent(agentState.ID, func(a *state.AgentState) error {
			a.Status = agentState.Status
			a.Team = agentState.Team
			return nil
		}); err != nil {
			logger.Debug("replicated agent state apply skipped", "agent_id", agentState.ID, "error", err)
		}
	}); err != nil {
		logger.Warn("failed to subscribe to cluster state updates", "error", err)
	}

	// Register control message handler for hivectl commands.
	// hivectl sends requests on hive.ctl.agents.* and hived responds via
	// the NATS request/reply pattern, ensuring all state mutations go through
	// hived's single VM manager and state store.
	ctlHandler := newControlHandler(controlHandlerConfig{
		RunCtx:       runCtx,
		ClusterRoot:  absRoot,
		Store:        store,
		AgentMgr:     agentMgr,
		Scheduler:    sched,
		Monitor:      monitor,
		Reconciler:   rec,
		ClusterMgr:   clusterMgr,
		Authorizer:   authorizer,
		Events:       eventPub,
		NodeRegistry: nodeRegistry,
		RateLimiter:  rateLimiter,
		Metrics:      collector,
		NATSConn:     nc,
		Logger:       logger,
	})
	if err := ctlHandler.subscribe(nc); err != nil {
		return fmt.Errorf("registering control handler: %w", err)
	}

	if cfg.Spec.Dashboard.Enabled {
		if cfg.Spec.Dashboard.AuthToken == "" {
			logger.Warn("dashboard auth token is empty — dashboard API is unauthenticated; set spec.dashboard.authToken in cluster.yaml for production")
		}
		dashSrv := dashboard.NewServer(dashboard.Config{
			Store:      store,
			NATSConn:   nc,
			Logs:       logAgg,
			Logger:     logger,
			Addr:       cfg.Spec.Dashboard.EffectiveAddr(),
			CORSOrigin: cfg.Spec.Dashboard.CORSOrigin,
			AuthToken:  cfg.Spec.Dashboard.AuthToken,
			Authorizer: authorizer,
		})
		// dashErrCh receives an error if Start() fails (e.g., port already
		// in use). The goroutine exits when Start() returns, so there is no
		// goroutine leak.
		dashErrCh := make(chan error, 1)
		go func() {
			if err := dashSrv.Start(); err != nil {
				logger.Error("dashboard server error", "error", err)
				dashErrCh <- err
			}
		}()
		defer func() {
			dashCtx, dashCancel := context.WithTimeout(context.Background(), dashboardShutdownTimeout)
			defer dashCancel()
			dashSrv.Stop(dashCtx) //nolint:errcheck // best-effort shutdown on exit
		}()
		// Wait for the dashboard server to bind its listener. This is a
		// startup race workaround: the dashboard package does not expose a
		// readiness signal, so we use a timeout instead. If the port is
		// already in use, the error appears almost immediately; the longer
		// timeout (2s) accommodates slower systems without adding a proper
		// ready channel to the dashboard server.
		select {
		case err := <-dashErrCh:
			return fmt.Errorf("dashboard server failed to start: %w", err)
		case <-time.After(dashboardStartupWait):
			// Server is likely listening.
		}
		logger.Info("dashboard started", "addr", cfg.Spec.Dashboard.EffectiveAddr())
	}

	if cfg.Spec.Metrics.Enabled {
		metricsAddr := cfg.Spec.Metrics.EffectiveAddr()
		metricsSrv := &http.Server{
			Addr:              metricsAddr,
			Handler:           collector.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		var metricsWg sync.WaitGroup
		metricsWg.Add(1)
		go func() {
			defer metricsWg.Done()
			logger.Info("metrics server starting", "addr", metricsAddr)
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("metrics server error", "error", err)
			}
		}()
		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), metricsShutdownTimeout)
			defer shutdownCancel()
			if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
				logger.Error("metrics server shutdown error", "error", err)
			}
			metricsWg.Wait()
		}()
	}

	logger.Info("hived is ready",
		"nats_url", ns.ClientURL(),
		"state_path", statePath,
	)

	// Notify systemd that hived is ready (Type=notify). No-op if
	// NOTIFY_SOCKET is not set (i.e., not running under systemd).
	sdNotify(logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case <-ctx.Done():
	case <-natsClosedCh:
		logger.Warn("NATS connection permanently closed, initiating shutdown")
	}
	logger.Info("received shutdown signal")

	graceful, err := production.NewGracefulShutdown(production.ShutdownConfig{
		Store:     store,
		VMManager: agentMgr,
		Logger:    logger,
		Timeout:   gracefulShutdownTimeout,
	})
	if err != nil {
		logger.Error("failed to create graceful shutdown handler", "error", err)
	} else {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), gracefulShutdownTimeout)
		defer shutdownCancel()
		if err := graceful.Execute(shutdownCtx); err != nil {
			logger.Error("graceful shutdown error", "error", err)
		}
	}

	return nil
}

// controlHandler handles NATS control messages from hivectl.
type controlHandler struct {
	runCtx       context.Context
	clusterRoot  string
	store        *state.Store
	agentMgr     *backend.AgentManager
	scheduler    *scheduler.Scheduler
	monitor      *health.Monitor
	reconciler   *reconciler.Reconciler
	clusterMgr   *cluster.Cluster // cluster manager for state replication
	authMu       sync.Mutex       // protects authorizer reads and writes
	authorizer   *auth.Authorizer // nil means RBAC disabled (backward compatible)
	events       *events.Publisher
	nodeRegistry *node.Registry
	rateLimiter  *production.RateLimiter
	metrics      *metrics.Collector
	natsConn     *nats.Conn
	logger       *slog.Logger
}

func newControlHandler(cfg controlHandlerConfig) *controlHandler {
	return &controlHandler{
		runCtx:       cfg.RunCtx,
		clusterRoot:  cfg.ClusterRoot,
		store:        cfg.Store,
		agentMgr:     cfg.AgentMgr,
		scheduler:    cfg.Scheduler,
		monitor:      cfg.Monitor,
		reconciler:   cfg.Reconciler,
		clusterMgr:   cfg.ClusterMgr,
		authorizer:   cfg.Authorizer,
		events:       cfg.Events,
		nodeRegistry: cfg.NodeRegistry,
		rateLimiter:  cfg.RateLimiter,
		metrics:      cfg.Metrics,
		natsConn:     cfg.NATSConn,
		logger:       cfg.Logger,
	}
}

// rebuildAuth rebuilds the RBAC authorizer from the current state store.
// This must be called after any user mutation (create, update, revoke, rotate)
// so that authentication and authorization reflect the latest user data.
// When users exist in the store, the authorizer is (re)created; when no users
// remain, the authorizer is set to nil to restore backward-compatible mode.
func (h *controlHandler) rebuildAuth() {
	h.authMu.Lock()
	defer h.authMu.Unlock()

	storedUsers := h.store.AllUsers()
	if len(storedUsers) == 0 {
		h.authorizer = nil
		h.logger.Info("RBAC authorizer cleared (no users remain)")
		return
	}
	users := make([]auth.User, len(storedUsers))
	for i, u := range storedUsers {
		users[i] = *u
	}
	h.authorizer = auth.NewAuthorizer(users, h.logger)
	h.logger.Info("RBAC authorizer rebuilt", "user_count", len(users))
}

// subscribe registers NATS subscriptions for all control subjects.
// Each handler is wrapped with rate limiting to prevent abuse.
func (h *controlHandler) subscribe(nc *nats.Conn) error {
	subjects := map[string]nats.MsgHandler{
		protocol.SubjAgentStart:   h.handleStart,
		protocol.SubjAgentStop:    h.handleStop,
		protocol.SubjAgentRestart: h.handleRestart,
		protocol.SubjAgentDestroy: h.handleDestroy,
		protocol.SubjAgentStatus:  h.handleStatus,
		protocol.SubjAgentList:    h.handleList,
		// Node handlers
		protocol.SubjNodeList:     h.handleNodesList,
		protocol.SubjNodeStatus:   h.handleNodesStatus,
		protocol.SubjNodeDrain:    h.handleNodesDrain,
		protocol.SubjNodeCordon:   h.handleNodesCordon,
		protocol.SubjNodeUncordon: h.handleNodesUncordon,
		protocol.SubjNodeLabel:    h.handleNodesLabel,
		protocol.SubjNodeUnlabel:  h.handleNodesUnlabel,
		protocol.SubjNodeApprove:  h.handleNodesApprove,
		protocol.SubjNodeRemove:   h.handleNodesRemove,
		// Token handlers
		protocol.SubjTokenCreate: h.handleTokensCreate,
		protocol.SubjTokenList:   h.handleTokensList,
		protocol.SubjTokenRevoke: h.handleTokensRevoke,
		// User handlers
		protocol.SubjUserCreate: h.handleUsersCreate,
		protocol.SubjUserList:   h.handleUsersList,
		protocol.SubjUserUpdate: h.handleUsersUpdate,
		protocol.SubjUserRevoke: h.handleUsersRevoke,
		protocol.SubjUserRotate: h.handleUsersRotate,
		// Capability handlers
		protocol.SubjCapabilityList:      h.handleCapabilitiesList,
		protocol.SubjCapabilityDescribe:  h.handleCapabilitiesDescribe,
		protocol.SubjCapabilityProviders: h.handleCapabilitiesProviders,
		// Cluster status handler
		protocol.SubjStatus: h.handleClusterStatus,
	}

	for subject, handler := range subjects {
		subj := subject         // capture for closure
		innerHandler := handler // capture for closure
		wrappedHandler := func(msg *nats.Msg) {
			if h.rateLimiter != nil && !h.rateLimiter.Allow(subj) {
				h.respondError(msg, "rate limit exceeded")
				return
			}
			innerHandler(msg)
		}
		if _, err := nc.Subscribe(subject, wrappedHandler); err != nil {
			return fmt.Errorf("subscribing to %s: %w", subject, err)
		}
		h.logger.Info("subscribed to control subject", "subject", subject)
	}

	return nil
}

// parseRequest extracts the CtlRequest and envelope from a NATS message.
func (h *controlHandler) parseRequest(msg *nats.Msg) (*protocol.CtlRequest, *types.Envelope, error) {
	var env types.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		return nil, nil, fmt.Errorf("parsing envelope: %w", err)
	}

	if err := env.Validate(); err != nil {
		return nil, nil, err
	}

	var req protocol.CtlRequest
	if err := json.Unmarshal(env.Payload, &req); err != nil {
		return nil, nil, fmt.Errorf("parsing control request: %w", err)
	}

	return &req, &env, nil
}

// authorize checks RBAC permissions for the request. It returns nil if
// the operation is allowed. If no authorizer is configured (single-user mode),
// all operations are allowed for backward compatibility. When RBAC is enabled,
// requests must include a valid UserToken or they will be rejected.
func (h *controlHandler) authorize(env *types.Envelope, action string, resource string) error {
	h.authMu.Lock()
	authorizer := h.authorizer
	h.authMu.Unlock()

	if authorizer == nil {
		// RBAC not configured; allow all operations.
		return nil
	}

	if env.UserToken == "" {
		// RBAC is enabled (authorizer is non-nil) but no credentials were
		// provided. Reject the request to prevent authorization bypass.
		return fmt.Errorf("authentication required: no token provided")
	}

	user, err := authorizer.Authenticate(env.UserToken)
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	if err := authorizer.Authorize(user, action, resource); err != nil {
		h.logger.Warn("authorization denied",
			"user_id", user.ID,
			"role", user.Role,
			"action", action,
			"resource", resource,
		)
		return err
	}

	h.logger.Debug("authorization granted",
		"user_id", user.ID,
		"role", user.Role,
		"action", action,
		"resource", resource,
	)
	return nil
}

// replicateAgentState replicates the current state of an agent to other cluster
// nodes via the cluster manager. This is called after agent state transitions
// (start, stop, destroy, scale) to keep worker nodes in sync.
func (h *controlHandler) replicateAgentState(agentID string) {
	if h.clusterMgr == nil {
		return
	}
	agent := h.store.GetAgent(agentID)
	if agent == nil {
		return
	}
	data, err := json.Marshal(agent)
	if err != nil {
		h.logger.Warn("failed to marshal agent state for replication",
			"agent_id", agentID,
			"error", err,
		)
		return
	}
	if err := h.clusterMgr.ReplicateAgentState(agentID, data); err != nil {
		h.logger.Warn("failed to replicate agent state", "agent_id", agentID, "error", err)
	}
}

// respond sends a CtlResponse back on the reply subject.
func (h *controlHandler) respond(msg *nats.Msg, resp *protocol.CtlResponse) {
	data, err := json.Marshal(resp)
	if err != nil {
		h.logger.Error("failed to marshal control response", "error", err)
		return
	}
	if err := msg.Respond(data); err != nil {
		h.logger.Error("failed to send control response", "error", err)
	}
}

// respondError sends an error response.
func (h *controlHandler) respondError(msg *nats.Msg, errMsg string) {
	h.respond(msg, &protocol.CtlResponse{Success: false, Error: errMsg})
}

// respondData sends a success response with arbitrary typed data in the Data field.
func (h *controlHandler) respondData(msg *nats.Msg, v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		h.respondError(msg, fmt.Sprintf("marshaling response data: %v", err))
		return
	}
	h.respond(msg, &protocol.CtlResponse{Success: true, Data: data})
}

// withAuth handles the common request parsing and authorization preamble.
// It parses the request, authorizes with the given action and static resource,
// and calls fn on success.
func (h *controlHandler) withAuth(msg *nats.Msg, action, resource string, fn func(req *protocol.CtlRequest, env *types.Envelope)) {
	req, env, err := h.parseRequest(msg)
	if err != nil {
		h.logger.Warn("invalid request", "error", err)
		h.respondError(msg, "invalid request")
		return
	}
	if err := h.authorize(env, action, resource); err != nil {
		h.respondError(msg, fmt.Sprintf("unauthorized: %v", err))
		return
	}
	fn(req, env)
}

// withAgentAuth handles request parsing, agent_id validation, and authorization
// using req.AgentID as the RBAC resource.
func (h *controlHandler) withAgentAuth(msg *nats.Msg, action string, fn func(req *protocol.CtlRequest, env *types.Envelope)) {
	req, env, err := h.parseRequest(msg)
	if err != nil {
		h.logger.Warn("invalid request", "error", err)
		h.respondError(msg, "invalid request")
		return
	}
	if err := types.ValidateSubjectComponent("agent_id", req.AgentID); err != nil {
		h.respondError(msg, fmt.Sprintf("invalid agent_id: %v", err))
		return
	}
	if err := h.authorize(env, action, req.AgentID); err != nil {
		h.respondError(msg, fmt.Sprintf("unauthorized: %v", err))
		return
	}
	fn(req, env)
}

// validateID validates an ID field and sends an error response on failure.
func (h *controlHandler) validateID(msg *nats.Msg, fieldName, value string) bool {
	if err := types.ValidateSubjectComponent(fieldName, value); err != nil {
		h.respondError(msg, fmt.Sprintf("invalid %s: %v", fieldName, err))
		return false
	}
	return true
}

func (h *controlHandler) handleStart(msg *nats.Msg) {
	h.withAgentAuth(msg, "start", func(req *protocol.CtlRequest, env *types.Envelope) {
		h.logger.Info("control: start agent", "agent_id", req.AgentID)

		agents, err := config.LoadAgents(h.clusterRoot)
		if err != nil {
			h.logger.Error("loading agents", "error", err)
			h.respondError(msg, "internal error loading agent manifests")
			return
		}

		agent, ok := agents[req.AgentID]
		if !ok {
			h.respondError(msg, fmt.Sprintf("agent %q not found in manifests", req.AgentID))
			return
		}

		startCtx, startCancel := context.WithTimeout(h.runCtx, 2*time.Minute)
		defer startCancel()

		if err := h.agentMgr.StartAgent(startCtx, agent); err != nil {
			h.events.AgentFailed(req.AgentID, err.Error())
			h.respondError(msg, fmt.Sprintf("starting agent: %v", err))
			return
		}

		h.events.AgentStarted(req.AgentID)
		h.replicateAgentState(req.AgentID)
		h.respond(msg, &protocol.CtlResponse{Success: true})
	})
}

func (h *controlHandler) handleStop(msg *nats.Msg) {
	h.withAgentAuth(msg, "stop", func(req *protocol.CtlRequest, env *types.Envelope) {
		h.logger.Info("control: stop agent", "agent_id", req.AgentID)

		stopCtx, stopCancel := context.WithTimeout(h.runCtx, 2*time.Minute)
		defer stopCancel()
		if err := h.agentMgr.StopAgent(stopCtx, req.AgentID); err != nil {
			h.respondError(msg, fmt.Sprintf("stopping agent: %v", err))
			return
		}

		h.events.AgentStopped(req.AgentID)
		h.replicateAgentState(req.AgentID)
		h.respond(msg, &protocol.CtlResponse{Success: true})
	})
}

func (h *controlHandler) handleRestart(msg *nats.Msg) {
	h.withAgentAuth(msg, "restart", func(req *protocol.CtlRequest, env *types.Envelope) {
		h.logger.Info("control: restart agent", "agent_id", req.AgentID)

		agents, err := config.LoadAgents(h.clusterRoot)
		if err != nil {
			h.logger.Error("loading agents", "error", err)
			h.respondError(msg, "internal error loading agent manifests")
			return
		}

		agent, ok := agents[req.AgentID]
		if !ok {
			h.respondError(msg, fmt.Sprintf("agent %q not found in manifests", req.AgentID))
			return
		}

		restartCtx, restartCancel := context.WithTimeout(h.runCtx, 2*time.Minute)
		defer restartCancel()
		if err := h.agentMgr.RestartAgent(restartCtx, req.AgentID, agent); err != nil {
			h.respondError(msg, fmt.Sprintf("restarting agent: %v", err))
			return
		}

		h.events.AgentStarted(req.AgentID)
		h.replicateAgentState(req.AgentID)
		h.respond(msg, &protocol.CtlResponse{Success: true})
	})
}

func (h *controlHandler) handleDestroy(msg *nats.Msg) {
	h.withAgentAuth(msg, "destroy", func(req *protocol.CtlRequest, env *types.Envelope) {
		h.logger.Info("control: destroy agent", "agent_id", req.AgentID)

		destroyCtx, destroyCancel := context.WithTimeout(h.runCtx, 2*time.Minute)
		defer destroyCancel()
		if err := h.agentMgr.DestroyAgent(destroyCtx, req.AgentID); err != nil {
			h.respondError(msg, fmt.Sprintf("destroying agent: %v", err))
			return
		}

		// Release scheduler allocations so the in-memory resource tracking
		// stays accurate. Log but don't fail the destroy on error.
		if err := h.scheduler.ReleaseAgent(req.AgentID); err != nil {
			h.logger.Warn("scheduler release failed after destroy",
				"agent_id", req.AgentID,
				"error", err,
			)
		}
		// Remove agent from health monitor to prevent unbounded map growth.
		h.monitor.RemoveAgent(req.AgentID)
		// Clean up stale agent metric labels to prevent unbounded cardinality growth.
		h.metrics.RemoveAgent(req.AgentID)
		h.replicateAgentState(req.AgentID)

		h.respond(msg, &protocol.CtlResponse{Success: true})
	})
}

func (h *controlHandler) handleStatus(msg *nats.Msg) {
	h.withAgentAuth(msg, "status", func(req *protocol.CtlRequest, env *types.Envelope) {
		agent := h.store.GetAgent(req.AgentID)
		if agent == nil {
			h.respondError(msg, fmt.Sprintf("agent %q not found", req.AgentID))
			return
		}

		h.respond(msg, &protocol.CtlResponse{Success: true, Agent: agent})
	})
}

func (h *controlHandler) handleList(msg *nats.Msg) {
	h.withAuth(msg, "list", "", func(req *protocol.CtlRequest, env *types.Envelope) {
		agents := h.store.AllAgents()
		h.respond(msg, &protocol.CtlResponse{Success: true, Agents: agents})
	})
}

// schedulerAdapter wraps *scheduler.Scheduler to satisfy the
// reconciler.AgentScheduler interface, which expects (string, error)
// instead of (*Assignment, error).
type schedulerAdapter struct {
	s *scheduler.Scheduler
}

func (a *schedulerAdapter) Schedule(manifest *types.AgentManifest) (string, error) {
	assignment, err := a.s.Schedule(manifest)
	if err != nil {
		return "", err
	}
	// BackendType from the assignment is not forwarded to the reconciler because
	// AgentManager.StartAgent independently resolves the backend from
	// manifest.Spec.Runtime.Backend. The scheduler uses BackendType during node
	// validation (e.g., requiring KVM for firecracker) but the reconciler only
	// needs the target nodeID.
	return assignment.NodeID, nil
}

// --- Capability handlers ---

func (h *controlHandler) handleCapabilitiesList(msg *nats.Msg) {
	h.withAuth(msg, "view", "capabilities", func(req *protocol.CtlRequest, env *types.Envelope) {
		// GetCapabilityRegistry returns a deep copy; iterate a snapshot of keys
		// to avoid any issues if the underlying map were shared.
		registry := h.store.GetCapabilityRegistry()

		type capEntry struct {
			Name    string `json:"name"`
			AgentID string `json:"agent_id"`
			Team    string `json:"team,omitempty"`
		}

		// Snapshot agent IDs for deterministic, safe iteration.
		agentIDs := make([]string, 0, len(registry.Agents))
		for agentID := range registry.Agents {
			agentIDs = append(agentIDs, agentID)
		}

		var result []capEntry
		for _, agentID := range agentIDs {
			entry := registry.Agents[agentID]
			for _, cap := range entry.Capabilities {
				result = append(result, capEntry{
					Name:    cap.Name,
					AgentID: agentID,
					Team:    entry.TeamID,
				})
			}
		}

		h.respondData(msg, result)
	})
}

func (h *controlHandler) handleCapabilitiesDescribe(msg *nats.Msg) {
	h.withAuth(msg, "view", "capabilities", func(req *protocol.CtlRequest, env *types.Envelope) {
		// Re-parse payload for the capability name.
		var capReq struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(env.Payload, &capReq); err != nil {
			h.logger.Warn("invalid request payload", "error", err)
			h.respondError(msg, "invalid request payload")
			return
		}
		if err := types.ValidateSubjectComponent("capability", capReq.Name); err != nil {
			h.respondError(msg, fmt.Sprintf("invalid capability name: %v", err))
			return
		}

		registry := h.store.GetCapabilityRegistry()
		for _, entry := range registry.Agents {
			for _, cap := range entry.Capabilities {
				if cap.Name == capReq.Name {
					h.respondData(msg, cap)
					return
				}
			}
		}

		h.respondError(msg, fmt.Sprintf("capability %q not found", capReq.Name))
	})
}

// sdNotify sends READY=1 to systemd's notification socket if NOTIFY_SOCKET
// is set. This signals that hived is fully initialized and ready to accept
// connections, allowing dependent services (e.g., hive-agent-*) to start.
// No-op when not running under systemd or when Type != notify.
func sdNotify(logger *slog.Logger) {
	sock := os.Getenv("NOTIFY_SOCKET")
	if sock == "" {
		return
	}
	conn, err := net.Dial("unixgram", sock)
	if err != nil {
		logger.Warn("sd_notify: failed to connect", "socket", sock, "error", err)
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("READY=1")); err != nil {
		logger.Warn("sd_notify: failed to send READY", "error", err)
		return
	}
	logger.Info("sd_notify: notified systemd ready")
}

func (h *controlHandler) handleCapabilitiesProviders(msg *nats.Msg) {
	h.withAuth(msg, "view", "capabilities", func(req *protocol.CtlRequest, env *types.Envelope) {
		// Re-parse payload for the capability name.
		var capReq struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(env.Payload, &capReq); err != nil {
			h.logger.Warn("invalid request payload", "error", err)
			h.respondError(msg, "invalid request payload")
			return
		}
		if err := types.ValidateSubjectComponent("capability", capReq.Name); err != nil {
			h.respondError(msg, fmt.Sprintf("invalid capability name: %v", err))
			return
		}

		registry := h.store.GetCapabilityRegistry()

		type providerEntry struct {
			AgentID string `json:"agent_id"`
			Team    string `json:"team,omitempty"`
			Status  string `json:"status"`
		}

		var providers []providerEntry
		for agentID, entry := range registry.Agents {
			for _, cap := range entry.Capabilities {
				if cap.Name == capReq.Name {
					status := "unknown"
					if agent := h.store.GetAgent(agentID); agent != nil {
						status = string(agent.Status)
					}
					providers = append(providers, providerEntry{
						AgentID: agentID,
						Team:    entry.TeamID,
						Status:  status,
					})
					break
				}
			}
		}

		h.respondData(msg, providers)
	})
}
