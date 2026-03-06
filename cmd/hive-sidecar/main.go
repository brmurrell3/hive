package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/brmurrell3/hive/internal/sidecar"
)

func main() {
	// When running as PID 1 inside a Firecracker VM, orphaned child
	// processes get re-parented to us. We must reap them to prevent
	// zombie accumulation. On non-Linux this is a no-op.
	startReaper()

	var (
		agentID        = flag.String("agent-id", "", "Agent ID (required)")
		teamID         = flag.String("team-id", "", "Team ID")
		natsURL        = flag.String("nats-url", "nats://localhost:4222", "NATS server URL")
		natsToken      = flag.String("nats-token", "", "NATS auth token (if the server requires authentication)")
		httpAddr       = flag.String("http-addr", ":9100", "HTTP API listen address")
		runtimeCmd     = flag.String("runtime-cmd", "", "Command to start the agent runtime")
		runtimeArgs    = flag.String("runtime-args", "", "Comma-separated arguments for runtime command")
		workspace      = flag.String("workspace", "/workspace", "Working directory for the runtime process")
		healthInterval = flag.Duration("health-interval", 30*time.Second, "Interval between heartbeat publications")
		tier           = flag.String("tier", "vm", "Agent tier (vm, native, firmware)")
		vsock          = flag.Bool("vsock", false, "Enable vsock-to-TCP proxy for NATS connectivity (Firecracker VM mode)")
		vsockPort      = flag.Uint("vsock-port", 4222, "Vsock port on the host for NATS (used with --vsock)")
		// capabilitiesJSON accepts a JSON array of capability objects so the
		// sidecar knows which capabilities to register with the router.
		// Example: --capabilities '[{"name":"summarize","description":"Summarize text"}]'
		capabilitiesJSON = flag.String("capabilities", "", "JSON array of capability objects (e.g. '[{\"name\":\"foo\",\"description\":\"bar\"}]')")
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

	// Parse capabilities from the JSON flag.
	var caps []sidecar.Capability
	if *capabilitiesJSON != "" {
		if err := json.Unmarshal([]byte(*capabilitiesJSON), &caps); err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid --capabilities JSON: %v\n", err)
			os.Exit(1)
		}
		logger.Info("loaded capabilities from --capabilities flag", "count", len(caps))
	}

	cfg := sidecar.Config{
		AgentID:        *agentID,
		TeamID:         *teamID,
		NATSUrl:        *natsURL,
		NATSToken:      *natsToken,
		HTTPAddr:       *httpAddr,
		RuntimeCmd:     *runtimeCmd,
		RuntimeArgs:    args,
		WorkspacePath:  *workspace,
		HealthInterval: *healthInterval,
		Tier:           *tier,
		Mode:           sidecar.ModeStandalone,
		Capabilities:   caps,
		VsockEnabled:   *vsock,
		VsockHostCID:   2, // VMADDR_CID_HOST
		VsockHostPort:  uint32(*vsockPort),
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
