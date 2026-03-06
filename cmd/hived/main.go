package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/brmurrell3/hive/internal/auth"
	"github.com/brmurrell3/hive/internal/capability"
	"github.com/brmurrell3/hive/internal/cluster"
	"github.com/brmurrell3/hive/internal/config"
	"github.com/brmurrell3/hive/internal/dashboard"
	"github.com/brmurrell3/hive/internal/director"
	"github.com/brmurrell3/hive/internal/firmware"
	"github.com/brmurrell3/hive/internal/health"
	"github.com/brmurrell3/hive/internal/logs"
	"github.com/brmurrell3/hive/internal/metrics"
	"github.com/brmurrell3/hive/internal/mqtt"
	hivenats "github.com/brmurrell3/hive/internal/nats"
	"github.com/brmurrell3/hive/internal/node"
	"github.com/brmurrell3/hive/internal/production"
	"github.com/brmurrell3/hive/internal/reconciler"
	"github.com/brmurrell3/hive/internal/scheduler"
	"github.com/brmurrell3/hive/internal/state"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/brmurrell3/hive/internal/vm"
	"github.com/brmurrell3/hive/internal/watcher"
	"github.com/nats-io/nats.go"
)

var version = "dev"

func main() {
	clusterRoot := flag.String("cluster-root", ".", "Path to the cluster root directory")
	showVersion := flag.Bool("version", false, "Print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Printf("hived %s\n", version)
		os.Exit(0)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	if err := run(*clusterRoot, logger); err != nil {
		logger.Error("hived failed", "error", err)
		os.Exit(1)
	}
}

func run(clusterRoot string, logger *slog.Logger) error {
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

	// Determine JetStream data directory
	dataDir := filepath.Join(absRoot, ".state", "jetstream")
	if cfg.Spec.NATS.JetStream.StorePath != "" {
		dataDir = cfg.Spec.NATS.JetStream.StorePath
	}

	// Start embedded NATS server.
	ns, err := hivenats.NewServer(cfg.Spec.NATS, dataDir, logger)
	if err != nil {
		return fmt.Errorf("creating NATS server: %w", err)
	}
	if err := ns.Start(); err != nil {
		return fmt.Errorf("starting NATS server: %w", err)
	}
	defer ns.Shutdown()

	// Initialize state store.
	statePath := filepath.Join(absRoot, "state.db")
	store, err := state.NewStore(statePath, logger)
	if err != nil {
		return fmt.Errorf("initializing state store: %w", err)
	}
	defer store.Close()

	// Write the NATS auth token to .state/nats-auth-token so hivectl can read it.
	stateDir := filepath.Join(absRoot, ".state")
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}
	tokenPath := filepath.Join(stateDir, "nats-auth-token")
	if err := os.WriteFile(tokenPath, []byte(ns.AuthToken()), 0600); err != nil {
		return fmt.Errorf("writing NATS auth token: %w", err)
	}
	logger.Info("NATS auth token written", "path", tokenPath)

	// Connect to the embedded NATS server as a client using the auth token.
	nc, err := nats.Connect(ns.ClientURL(),
		nats.Token(ns.AuthToken()),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(1*time.Second),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			logger.Warn("NATS client disconnected", "error", err)
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			logger.Info("NATS client reconnected")
		}),
	)
	if err != nil {
		return fmt.Errorf("connecting to embedded NATS: %w", err)
	}
	defer nc.Drain()

	// Start metrics collector (no deps).
	collector := metrics.NewCollector()
	logger.Info("metrics collector initialized")

	// Start log aggregator.
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

	// Start MQTT bridge (if enabled).
	if cfg.Spec.MQTT.Enabled {
		mqttBridge := mqtt.NewBridge(mqtt.Config{
			Port:     cfg.Spec.MQTT.EffectivePort(),
			NATSConn: nc,
			Store:    store,
			Logger:   logger,
		})
		if err := mqttBridge.Start(); err != nil {
			return fmt.Errorf("starting MQTT bridge: %w", err)
		}
		defer mqttBridge.Stop()
		logger.Info("MQTT bridge started", "port", mqttBridge.Port())
	}

	// Determine hypervisor (mock if no KVM or env override).
	var hyp vm.Hypervisor
	if os.Getenv("HIVE_TEST_FIRECRACKER") == "mock" {
		hyp = vm.NewMockHypervisor()
		logger.Info("using mock hypervisor")
	} else {
		fcHyp, err := vm.NewFirecrackerHypervisor("", logger)
		if err != nil {
			return fmt.Errorf("initializing Firecracker hypervisor: %w", err)
		}
		hyp = fcHyp
	}

	// Initialize VM manager.
	// Pass the NATS port and auth token so the manager can set up vsock
	// forwarding for VMs and write the token into sidecar.conf.
	natsPort := cfg.Spec.NATS.Port
	if natsPort == 0 {
		natsPort = 4222
	}
	vmMgr := vm.NewManager(absRoot, store, logger, hyp, uint32(natsPort), ns.AuthToken())
	if err := vmMgr.ReconcileOnStartup(); err != nil {
		logger.Error("startup reconciliation error", "error", err)
	}

	// Start health monitor.
	monitor := health.NewMonitor(
		store, nc,
		cfg.Spec.Defaults.Health.Interval,
		cfg.Spec.Defaults.Health.MaxFailures,
		logger,
	)

	// Connect restart manager to health monitor.
	restartMgr := health.NewRestartManager(store, vmMgr, logger)
	monitor.SetRestartManager(restartMgr)

	if err := monitor.Start(); err != nil {
		return fmt.Errorf("starting health monitor: %w", err)
	}
	defer monitor.Stop()

	// Start capability router for hived's own capabilities.
	capRouter := capability.NewRouter("hived", nc, logger)
	if err := capRouter.Start(); err != nil {
		return fmt.Errorf("starting capability router: %w", err)
	}
	defer capRouter.Stop()

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

	// Start node registry so that join tokens and hive.join.request work.
	nodeRegistry := node.NewRegistry(store, nc, logger, advertiseAddr)
	if err := nodeRegistry.Start(); err != nil {
		return fmt.Errorf("starting node registry: %w", err)
	}
	defer nodeRegistry.Stop()

	// Load desired state (agents + teams + cluster config with merged defaults)
	// so we can populate RestartManager configs for every agent.
	desiredState, err := config.LoadDesiredState(absRoot)
	if err != nil {
		// Non-fatal: agents directory may not exist yet on a fresh cluster.
		logger.Warn("could not load desired state for restart configs", "error", err)
	} else {
		for agentID, manifest := range desiredState.Agents {
			restartMgr.SetConfig(agentID, health.RestartConfig{
				Policy:      manifest.Spec.Restart.Policy,
				MaxRestarts: manifest.Spec.Restart.MaxRestarts,
				Backoff:     manifest.Spec.Restart.Backoff,
				Manifest:    manifest,
			})
		}
		logger.Info("restart configs populated", "agent_count", len(desiredState.Agents))
	}

	// Initialize scheduler (stateless, no Start/Stop).
	sched := scheduler.NewScheduler(store, logger)
	logger.Info("scheduler initialized")

	// Start reconciler.
	rec := reconciler.NewReconciler(store, absRoot, logger)
	rec.SetScheduler(&schedulerAdapter{s: sched})
	rec.SetActionHandler(func(action reconciler.Action) error {
		switch action.Type {
		case reconciler.ActionCreate:
			return vmMgr.StartAgent(action.Manifest)
		case reconciler.ActionDestroy:
			if err := vmMgr.DestroyAgent(action.AgentID); err != nil {
				return err
			}
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
			return nil
		case reconciler.ActionRestart:
			return vmMgr.RestartAgent(action.AgentID, action.Manifest)
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
		subject := fmt.Sprintf("hive.agent.%s.memory", agentID)
		env := types.Envelope{
			ID:        types.NewUUID(),
			From:      "hived",
			To:        agentID,
			Type:      types.MessageTypeMemoryUpdate,
			Timestamp: time.Now().UTC(),
			Payload:   string(content),
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
	defer memWatcher.Stop()

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

	// Register control message handler for hivectl commands.
	// hivectl sends requests on hive.ctl.agents.* and hived responds via
	// the NATS request/reply pattern, ensuring all state mutations go through
	// hived's single VM manager and state store.
	ctlHandler := newControlHandler(absRoot, store, vmMgr, sched, monitor, authorizer, logger)
	if err := ctlHandler.subscribe(nc); err != nil {
		return fmt.Errorf("registering control handler: %w", err)
	}

	// Run crash recovery to reconcile stale state from a previous unclean shutdown.
	crashRecovery := production.NewCrashRecovery(production.RecoveryConfig{
		Store:  store,
		Logger: logger,
	})
	if err := crashRecovery.Reconcile(); err != nil {
		logger.Error("crash recovery reconciliation error", "error", err)
	}

	// Start resource monitor.
	resMon := production.NewResourceMonitor(production.MonitorConfig{
		Store:           store,
		Metrics:         collector,
		Logger:          logger,
		CheckInterval:   30 * time.Second,
		MemoryThreshold: 0.9,
		CPUThreshold:    0.9,
	})
	if err := resMon.Start(); err != nil {
		return fmt.Errorf("starting resource monitor: %w", err)
	}
	defer resMon.Stop()

	// Start cluster manager.
	clusterRole := cluster.Role("root")
	clusterMgr := cluster.NewCluster(cluster.Config{
		Role:        clusterRole,
		NATSMode:    cfg.Spec.NATS.Mode,
		NATSUrls:    cfg.Spec.NATS.URLs,
		ClusterRoot: absRoot,
	}, store, logger)
	if err := clusterMgr.Start(); err != nil {
		return fmt.Errorf("starting cluster manager: %w", err)
	}
	defer clusterMgr.Stop()

	logger.Info("scheduler initialized")

	// Start cross-team router and director (if teams exist).
	if desiredState != nil && len(desiredState.Teams) > 0 {
		ctRouter := capability.NewCrossTeamRouter(nc, store, logger)
		if err := ctRouter.Start(desiredState.Teams); err != nil {
			return fmt.Errorf("starting cross-team router: %w", err)
		}
		defer ctRouter.Stop()
		logger.Info("cross-team router started", "team_count", len(desiredState.Teams))

		// Build a director auth function from the RBAC authorizer when available.
		// The director's AuthFunc takes a senderID (envelope From field) and
		// verifies the sender is a known user in the RBAC system.
		var directorAuth director.AuthFunc
		if authorizer != nil {
			directorAuth = func(senderID string) error {
				user := authorizer.GetUser(senderID)
				if user == nil {
					return fmt.Errorf("unknown sender %q", senderID)
				}
				return nil
			}
		}
		dir := director.NewDirector("director", nc, store, logger, directorAuth)
		if err := dir.Start(); err != nil {
			return fmt.Errorf("starting director: %w", err)
		}
		defer dir.Stop()
		logger.Info("director started")
	}

	// Initialize OTA updater (stateless, no Start/Stop).
	_ = firmware.NewUpdater(nc, logger)
	logger.Info("OTA updater initialized")

	// Start dashboard (if enabled).
	if cfg.Spec.Dashboard.Enabled {
		dashSrv := dashboard.NewServer(dashboard.Config{
			Store:      store,
			NATSConn:   nc,
			Logs:       logAgg,
			Logger:     logger,
			Addr:       cfg.Spec.Dashboard.EffectiveAddr(),
			CORSOrigin: cfg.Spec.Dashboard.CORSOrigin,
			AuthToken:  cfg.Spec.Dashboard.AuthToken,
		})
		go func() {
			if err := dashSrv.Start(); err != nil {
				logger.Error("dashboard server error", "error", err)
			}
		}()
		defer dashSrv.Stop(context.Background())
		logger.Info("dashboard started", "addr", cfg.Spec.Dashboard.EffectiveAddr())
	}

	// Start metrics HTTP server (if enabled).
	if cfg.Spec.Metrics.Enabled {
		metricsAddr := cfg.Spec.Metrics.EffectiveAddr()
		metricsSrv := &http.Server{
			Addr:              metricsAddr,
			Handler:           collector.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			logger.Info("metrics server starting", "addr", metricsAddr)
			if err := metricsSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logger.Error("metrics server error", "error", err)
			}
		}()
		defer metricsSrv.Close()
	}

	logger.Info("hived is ready",
		"nats_url", ns.ClientURL(),
		"state_path", statePath,
	)

	// Wait for shutdown signal, then execute graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()
	logger.Info("received shutdown signal")

	vmShutdown := &vmAdapter{mgr: vmMgr}
	graceful := production.NewGracefulShutdown(production.ShutdownConfig{
		Store:     store,
		VMManager: vmShutdown,
		Logger:    logger,
		Timeout:   30 * time.Second,
	})
	if err := graceful.Execute(context.Background()); err != nil {
		logger.Error("graceful shutdown error", "error", err)
	}

	return nil
}

// vmAdapter adapts vm.Manager to the production.VMAccess interface.
type vmAdapter struct{ mgr *vm.Manager }

func (a *vmAdapter) StopVM(_ context.Context, agentID string) error {
	return a.mgr.StopAgent(agentID)
}

// ctlRequest is the payload sent from hivectl to hived on control subjects.
type ctlRequest struct {
	AgentID string `json:"agent_id"`
}

// ctlResponse is the payload returned from hived to hivectl.
type ctlResponse struct {
	Success bool                `json:"success"`
	Error   string              `json:"error,omitempty"`
	Agent   *state.AgentState   `json:"agent,omitempty"`
	Agents  []*state.AgentState `json:"agents,omitempty"`
}

// controlHandler handles NATS control messages from hivectl.
type controlHandler struct {
	clusterRoot string
	store       *state.Store
	vmMgr       *vm.Manager
	scheduler   *scheduler.Scheduler
	monitor     *health.Monitor
	authorizer  *auth.Authorizer // nil means RBAC disabled (backward compatible)
	logger      *slog.Logger
}

func newControlHandler(clusterRoot string, store *state.Store, vmMgr *vm.Manager, sched *scheduler.Scheduler, mon *health.Monitor, authorizer *auth.Authorizer, logger *slog.Logger) *controlHandler {
	return &controlHandler{
		clusterRoot: clusterRoot,
		store:       store,
		vmMgr:       vmMgr,
		scheduler:   sched,
		monitor:     mon,
		authorizer:  authorizer,
		logger:      logger,
	}
}

// subscribe registers NATS subscriptions for all control subjects.
func (h *controlHandler) subscribe(nc *nats.Conn) error {
	subjects := map[string]nats.MsgHandler{
		"hive.ctl.agents.start":   h.handleStart,
		"hive.ctl.agents.stop":    h.handleStop,
		"hive.ctl.agents.restart": h.handleRestart,
		"hive.ctl.agents.destroy": h.handleDestroy,
		"hive.ctl.agents.status":  h.handleStatus,
		"hive.ctl.agents.list":    h.handleList,
	}

	for subject, handler := range subjects {
		if _, err := nc.Subscribe(subject, handler); err != nil {
			return fmt.Errorf("subscribing to %s: %w", subject, err)
		}
		h.logger.Info("subscribed to control subject", "subject", subject)
	}

	return nil
}

// parseRequest extracts the ctlRequest and envelope from a NATS message.
func (h *controlHandler) parseRequest(msg *nats.Msg) (*ctlRequest, *types.Envelope, error) {
	var env types.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		return nil, nil, fmt.Errorf("parsing envelope: %w", err)
	}

	// The Payload field is interface{}, re-marshal and unmarshal to get the typed request.
	payloadBytes, err := json.Marshal(env.Payload)
	if err != nil {
		return nil, nil, fmt.Errorf("re-marshaling payload: %w", err)
	}

	var req ctlRequest
	if err := json.Unmarshal(payloadBytes, &req); err != nil {
		return nil, nil, fmt.Errorf("parsing control request: %w", err)
	}

	return &req, &env, nil
}

// authorize checks RBAC permissions for the request. It returns nil if
// the operation is allowed. If no authorizer is configured (single-user mode),
// all operations are allowed for backward compatibility. When RBAC is enabled,
// requests must include a valid UserToken or they will be rejected.
func (h *controlHandler) authorize(env *types.Envelope, action string, resource string) error {
	if h.authorizer == nil {
		// RBAC not configured; allow all operations.
		return nil
	}

	if env.UserToken == "" {
		// RBAC is enabled (authorizer is non-nil) but no credentials were
		// provided. Reject the request to prevent authorization bypass.
		return fmt.Errorf("authentication required: no token provided")
	}

	user, err := h.authorizer.Authenticate(env.UserToken)
	if err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	if err := h.authorizer.Authorize(user, action, resource); err != nil {
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

// respond sends a ctlResponse back on the reply subject.
func (h *controlHandler) respond(msg *nats.Msg, resp *ctlResponse) {
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
	h.respond(msg, &ctlResponse{Success: false, Error: errMsg})
}

func (h *controlHandler) handleStart(msg *nats.Msg) {
	req, env, err := h.parseRequest(msg)
	if err != nil {
		h.respondError(msg, fmt.Sprintf("invalid request: %v", err))
		return
	}

	if err := h.authorize(env, "start", req.AgentID); err != nil {
		h.respondError(msg, fmt.Sprintf("unauthorized: %v", err))
		return
	}

	h.logger.Info("control: start agent", "agent_id", req.AgentID)

	// Load agent manifest from disk.
	agents, err := config.LoadAgents(h.clusterRoot)
	if err != nil {
		h.respondError(msg, fmt.Sprintf("loading agents: %v", err))
		return
	}

	agent, ok := agents[req.AgentID]
	if !ok {
		h.respondError(msg, fmt.Sprintf("agent %q not found in manifests", req.AgentID))
		return
	}

	if err := h.vmMgr.StartAgent(agent); err != nil {
		h.respondError(msg, fmt.Sprintf("starting agent: %v", err))
		return
	}

	h.respond(msg, &ctlResponse{Success: true})
}

func (h *controlHandler) handleStop(msg *nats.Msg) {
	req, env, err := h.parseRequest(msg)
	if err != nil {
		h.respondError(msg, fmt.Sprintf("invalid request: %v", err))
		return
	}

	if err := h.authorize(env, "stop", req.AgentID); err != nil {
		h.respondError(msg, fmt.Sprintf("unauthorized: %v", err))
		return
	}

	h.logger.Info("control: stop agent", "agent_id", req.AgentID)

	if err := h.vmMgr.StopAgent(req.AgentID); err != nil {
		h.respondError(msg, fmt.Sprintf("stopping agent: %v", err))
		return
	}

	h.respond(msg, &ctlResponse{Success: true})
}

func (h *controlHandler) handleRestart(msg *nats.Msg) {
	req, env, err := h.parseRequest(msg)
	if err != nil {
		h.respondError(msg, fmt.Sprintf("invalid request: %v", err))
		return
	}

	if err := h.authorize(env, "restart", req.AgentID); err != nil {
		h.respondError(msg, fmt.Sprintf("unauthorized: %v", err))
		return
	}

	h.logger.Info("control: restart agent", "agent_id", req.AgentID)

	// Load agent manifest for restart (needed to re-provision).
	agents, err := config.LoadAgents(h.clusterRoot)
	if err != nil {
		h.respondError(msg, fmt.Sprintf("loading agents: %v", err))
		return
	}

	agent, ok := agents[req.AgentID]
	if !ok {
		h.respondError(msg, fmt.Sprintf("agent %q not found in manifests", req.AgentID))
		return
	}

	if err := h.vmMgr.RestartAgent(req.AgentID, agent); err != nil {
		h.respondError(msg, fmt.Sprintf("restarting agent: %v", err))
		return
	}

	h.respond(msg, &ctlResponse{Success: true})
}

func (h *controlHandler) handleDestroy(msg *nats.Msg) {
	req, env, err := h.parseRequest(msg)
	if err != nil {
		h.respondError(msg, fmt.Sprintf("invalid request: %v", err))
		return
	}

	if err := h.authorize(env, "destroy", req.AgentID); err != nil {
		h.respondError(msg, fmt.Sprintf("unauthorized: %v", err))
		return
	}

	h.logger.Info("control: destroy agent", "agent_id", req.AgentID)

	if err := h.vmMgr.DestroyAgent(req.AgentID); err != nil {
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

	h.respond(msg, &ctlResponse{Success: true})
}

func (h *controlHandler) handleStatus(msg *nats.Msg) {
	req, env, err := h.parseRequest(msg)
	if err != nil {
		h.respondError(msg, fmt.Sprintf("invalid request: %v", err))
		return
	}

	if err := h.authorize(env, "status", req.AgentID); err != nil {
		h.respondError(msg, fmt.Sprintf("unauthorized: %v", err))
		return
	}

	agent := h.store.GetAgent(req.AgentID)
	if agent == nil {
		h.respondError(msg, fmt.Sprintf("agent %q not found", req.AgentID))
		return
	}

	h.respond(msg, &ctlResponse{Success: true, Agent: agent})
}

func (h *controlHandler) handleList(msg *nats.Msg) {
	_, env, err := h.parseRequest(msg)
	if err != nil {
		h.respondError(msg, fmt.Sprintf("invalid request: %v", err))
		return
	}

	if err := h.authorize(env, "list", ""); err != nil {
		h.respondError(msg, fmt.Sprintf("unauthorized: %v", err))
		return
	}

	agents := h.store.AllAgents()
	h.respond(msg, &ctlResponse{Success: true, Agents: agents})
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
	return assignment.NodeID, nil
}
