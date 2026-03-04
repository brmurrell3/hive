package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/hivehq/hive/internal/sidecar"
)

func main() {
	var (
		agentID        = flag.String("agent-id", "", "Agent ID (required)")
		teamID         = flag.String("team-id", "", "Team ID")
		natsURL        = flag.String("nats-url", "nats://localhost:4222", "NATS server URL")
		httpAddr       = flag.String("http-addr", ":9100", "HTTP API listen address")
		runtimeCmd     = flag.String("runtime-cmd", "", "Command to start the agent runtime")
		runtimeArgs    = flag.String("runtime-args", "", "Comma-separated arguments for runtime command")
		workspace      = flag.String("workspace", "/workspace", "Working directory for the runtime process")
		healthInterval = flag.Duration("health-interval", 30*time.Second, "Interval between heartbeat publications")
		tier           = flag.String("tier", "vm", "Agent tier (vm, native, firmware)")
	)

	flag.Parse()

	if *agentID == "" {
		fmt.Fprintln(os.Stderr, "error: --agent-id is required")
		flag.Usage()
		os.Exit(1)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))

	var args []string
	if *runtimeArgs != "" {
		args = strings.Split(*runtimeArgs, ",")
	}

	cfg := sidecar.Config{
		AgentID:        *agentID,
		TeamID:         *teamID,
		NATSUrl:        *natsURL,
		HTTPAddr:       *httpAddr,
		RuntimeCmd:     *runtimeCmd,
		RuntimeArgs:    args,
		WorkspacePath:  *workspace,
		HealthInterval: *healthInterval,
		Tier:           *tier,
		Mode:           sidecar.ModeStandalone,
	}

	s := sidecar.New(cfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		logger.Error("failed to start sidecar", "error", err)
		os.Exit(1)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	sig := <-sigCh
	logger.Info("received signal, shutting down", "signal", sig.String())

	if err := s.Stop(); err != nil {
		logger.Error("error during shutdown", "error", err)
		os.Exit(1)
	}
}
