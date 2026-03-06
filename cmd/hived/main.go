package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/hivehq/hive/internal/capability"
	"github.com/hivehq/hive/internal/config"
	"github.com/hivehq/hive/internal/health"
	hivenats "github.com/hivehq/hive/internal/nats"
	"github.com/hivehq/hive/internal/node"
	"github.com/hivehq/hive/internal/reconciler"
	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/types"
	"github.com/hivehq/hive/internal/vm"
	"github.com/hivehq/hive/internal/watcher"
	"github.com/nats-io/nats.go"
)

func main() {
	clusterRoot := flag.String("cluster-root", ".", "Path to the cluster root directory")
	flag.Parse()

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

	// T2-13: Initialize state store.
	statePath := filepath.Join(absRoot, "state.json")
	store, err := state.NewStore(statePath, logger)
	if err != nil {
		return fmt.Errorf("initializing state store: %w", err)
	}

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
	nc, err := nats.Connect(ns.ClientURL(), nats.Token(ns.AuthToken()))
	if err != nil {
		return fmt.Errorf("connecting to embedded NATS: %w", err)
	}
	defer nc.Drain()

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

	// T2-13: Initialize VM manager.
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

	// T2-13: Start health monitor.
	monitor := health.NewMonitor(
		store, nc,
		cfg.Spec.Defaults.Health.Interval,
		cfg.Spec.Defaults.Health.MaxFailures,
		logger,
	)

	// T2-01: Connect restart manager to health monitor.
	restartMgr := health.NewRestartManager(store, vmMgr, logger)
	monitor.SetRestartManager(restartMgr)

	if err := monitor.Start(); err != nil {
		return fmt.Errorf("starting health monitor: %w", err)
	}
	defer monitor.Stop()

	// T2-13: Start capability router for hived's own capabilities.
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

	// T2-13: Start reconciler.
	rec := reconciler.NewReconciler(store, absRoot, logger)
	rec.SetActionHandler(func(action reconciler.Action) error {
		switch action.Type {
		case reconciler.ActionCreate:
			return vmMgr.StartAgent(action.Manifest)
		case reconciler.ActionDestroy:
			return vmMgr.DestroyAgent(action.AgentID)
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

	// Register control message handler for hivectl commands.
	// hivectl sends requests on hive.ctl.agents.* and hived responds via
	// the NATS request/reply pattern, ensuring all state mutations go through
	// hived's single VM manager and state store.
	ctlHandler := newControlHandler(absRoot, store, vmMgr, logger)
	if err := ctlHandler.subscribe(nc); err != nil {
		return fmt.Errorf("registering control handler: %w", err)
	}

	logger.Info("hived is ready",
		"nats_url", ns.ClientURL(),
		"state_path", statePath,
	)

	// Wait for shutdown signal
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	<-ctx.Done()
	logger.Info("received shutdown signal")

	return nil
}

// ctlRequest is the payload sent from hivectl to hived on control subjects.
type ctlRequest struct {
	AgentID string `json:"agent_id"`
}

// ctlResponse is the payload returned from hived to hivectl.
type ctlResponse struct {
	Success bool              `json:"success"`
	Error   string            `json:"error,omitempty"`
	Agent   *state.AgentState `json:"agent,omitempty"`
	Agents  []*state.AgentState `json:"agents,omitempty"`
}

// controlHandler handles NATS control messages from hivectl.
type controlHandler struct {
	clusterRoot string
	store       *state.Store
	vmMgr       *vm.Manager
	logger      *slog.Logger
}

func newControlHandler(clusterRoot string, store *state.Store, vmMgr *vm.Manager, logger *slog.Logger) *controlHandler {
	return &controlHandler{
		clusterRoot: clusterRoot,
		store:       store,
		vmMgr:       vmMgr,
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

// parseRequest extracts the ctlRequest from a NATS envelope message.
func (h *controlHandler) parseRequest(msg *nats.Msg) (*ctlRequest, error) {
	var env types.Envelope
	if err := json.Unmarshal(msg.Data, &env); err != nil {
		return nil, fmt.Errorf("parsing envelope: %w", err)
	}

	// The Payload field is interface{}, re-marshal and unmarshal to get the typed request.
	payloadBytes, err := json.Marshal(env.Payload)
	if err != nil {
		return nil, fmt.Errorf("re-marshaling payload: %w", err)
	}

	var req ctlRequest
	if err := json.Unmarshal(payloadBytes, &req); err != nil {
		return nil, fmt.Errorf("parsing control request: %w", err)
	}

	return &req, nil
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
	req, err := h.parseRequest(msg)
	if err != nil {
		h.respondError(msg, fmt.Sprintf("invalid request: %v", err))
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
	req, err := h.parseRequest(msg)
	if err != nil {
		h.respondError(msg, fmt.Sprintf("invalid request: %v", err))
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
	req, err := h.parseRequest(msg)
	if err != nil {
		h.respondError(msg, fmt.Sprintf("invalid request: %v", err))
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
	req, err := h.parseRequest(msg)
	if err != nil {
		h.respondError(msg, fmt.Sprintf("invalid request: %v", err))
		return
	}

	h.logger.Info("control: destroy agent", "agent_id", req.AgentID)

	if err := h.vmMgr.DestroyAgent(req.AgentID); err != nil {
		h.respondError(msg, fmt.Sprintf("destroying agent: %v", err))
		return
	}

	h.respond(msg, &ctlResponse{Success: true})
}

func (h *controlHandler) handleStatus(msg *nats.Msg) {
	req, err := h.parseRequest(msg)
	if err != nil {
		h.respondError(msg, fmt.Sprintf("invalid request: %v", err))
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
	_, err := h.parseRequest(msg)
	if err != nil {
		h.respondError(msg, fmt.Sprintf("invalid request: %v", err))
		return
	}

	agents := h.store.AllAgents()
	h.respond(msg, &ctlResponse{Success: true, Agents: agents})
}
