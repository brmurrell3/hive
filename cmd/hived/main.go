package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/hivehq/hive/internal/capability"
	"github.com/hivehq/hive/internal/config"
	"github.com/hivehq/hive/internal/health"
	hivenats "github.com/hivehq/hive/internal/nats"
	"github.com/hivehq/hive/internal/reconciler"
	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/vm"
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

	// Connect to the embedded NATS server as a client.
	nc, err := nats.Connect(ns.ClientURL())
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
		hyp = vm.NewFirecrackerHypervisor("", logger)
	}

	// T2-13: Initialize VM manager.
	vmMgr := vm.NewManager(absRoot, store, logger, hyp)
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
