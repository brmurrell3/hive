// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/brmurrell3/hive/internal/logging"
	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/sidecar"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "hive-agent",
		Short: "Hive agent for Tier 2 devices",
	}

	rootCmd.AddCommand(joinCmd())
	rootCmd.AddCommand(versionCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("hive-agent %s %s/%s\n", version, runtime.GOOS, runtime.GOARCH)
			fmt.Printf("  Commit:     %s\n", commit)
			fmt.Printf("  Built:      %s\n", buildDate)
			fmt.Printf("  Go version: %s\n", runtime.Version())
		},
	}
}

func joinCmd() *cobra.Command {
	var (
		token        string
		controlPlane string
		agentID      string
		runtimeCmd   string
		runtimeArgs  string
		workDir      string
		httpAddr     string
		natsToken    string
		manifestPath string
		logLevel     string
	)

	cmd := &cobra.Command{
		Use:   "join",
		Short: "Join a Hive cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			level := logging.ParseLevel(logLevel)
			logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
				Level: level,
			}))

			// Fall back to environment variables for token flags.
			if token == "" {
				token = os.Getenv("HIVE_JOIN_TOKEN")
			}
			if natsToken == "" {
				natsToken = os.Getenv("HIVE_NATS_TOKEN")
			}

			if token == "" {
				return fmt.Errorf("--token is required (or set HIVE_JOIN_TOKEN)")
			}
			if controlPlane == "" {
				return fmt.Errorf("--control-plane is required")
			}
			// Strip scheme prefix if the user accidentally included one.
			// --control-plane expects HOST:PORT, not a full URL.
			if strings.Contains(controlPlane, "://") {
				controlPlane = strings.TrimPrefix(controlPlane, "nats://")
				controlPlane = strings.TrimPrefix(controlPlane, "tls://")
				if strings.Contains(controlPlane, "://") {
					return fmt.Errorf("--control-plane must be HOST:PORT, not a URL with a scheme")
				}
			}
			// Validate the control-plane address is a valid HOST:PORT.
			if _, _, err := net.SplitHostPort(controlPlane); err != nil {
				return fmt.Errorf("invalid control-plane address %q: %w", controlPlane, err)
			}

			if agentID == "" {
				return fmt.Errorf("--agent-id is required")
			}
			if err := types.ValidateSubjectComponent("agent-id", agentID); err != nil {
				return err
			}

			return runJoin(logger, JoinConfig{
				Token:        token,
				ControlPlane: controlPlane,
				AgentID:      agentID,
				RuntimeCmd:   runtimeCmd,
				RuntimeArgs:  runtimeArgs,
				WorkDir:      workDir,
				HTTPAddr:     httpAddr,
				NATSToken:    natsToken,
				ManifestPath: manifestPath,
			})
		},
	}

	cmd.Flags().StringVar(&token, "token", "", "Join token")
	cmd.Flags().StringVar(&controlPlane, "control-plane", "", "Control plane address (HOST:PORT)")
	cmd.Flags().StringVar(&agentID, "agent-id", "", "Agent ID for this node")
	cmd.Flags().StringVar(&runtimeCmd, "runtime-cmd", "", "Command to start the agent runtime")
	cmd.Flags().StringVar(&runtimeArgs, "runtime-args", "", "Comma-separated arguments for the runtime command")
	cmd.Flags().StringVar(&workDir, "work-dir", "/var/lib/hive/workspace", "Working directory for the runtime")
	cmd.Flags().StringVar(&httpAddr, "http-addr", ":9100", "HTTP API listen address")
	cmd.Flags().StringVar(&natsToken, "nats-token", "", "NATS authentication token")
	cmd.Flags().StringVar(&manifestPath, "manifest", "", "Path to agent manifest YAML (provides capabilities and team)")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")

	return cmd
}

// JoinConfig holds the configuration for joining a Hive cluster.
type JoinConfig struct {
	Token        string
	ControlPlane string
	AgentID      string
	RuntimeCmd   string
	RuntimeArgs  string
	WorkDir      string
	HTTPAddr     string
	NATSToken    string
	ManifestPath string
}

func runJoin(logger *slog.Logger, cfg JoinConfig) error {
	logger.Info("joining cluster",
		"control_plane", cfg.ControlPlane,
		"agent_id", cfg.AgentID,
	)

	// Build NATS URL from control plane address
	natsURL := fmt.Sprintf("nats://%s", cfg.ControlPlane)

	// Connect to NATS first to send the join request
	natsOpts := []nats.Option{
		nats.Name(fmt.Sprintf("hive-agent-%s", cfg.AgentID)),
		nats.ReconnectWait(2 * time.Second),
		nats.MaxReconnects(-1),
	}
	if cfg.NATSToken != "" {
		natsOpts = append(natsOpts, nats.Token(cfg.NATSToken))
	}

	joinNC, err := nats.Connect(natsURL, natsOpts...)
	if err != nil {
		return fmt.Errorf("connecting to NATS at %s: %w", natsURL, err)
	}

	// Collect hardware inventory
	inventory := collectHardwareInventory(logger)

	// Send join request
	joinReq := types.JoinRequest{
		Token:     cfg.Token,
		Hostname:  getHostname(logger),
		Arch:      runtime.GOARCH,
		Resources: inventory,
		AgentID:   cfg.AgentID,
	}

	reqData, err := json.Marshal(joinReq)
	if err != nil {
		if drainErr := joinNC.Drain(); drainErr != nil {
			logger.Warn("failed to drain join NATS connection", "error", drainErr)
		}
		return fmt.Errorf("marshaling join request: %w", err)
	}

	// Send request-reply
	msg, err := joinNC.Request(protocol.SubjJoinRequest, reqData, 10*time.Second)
	if err != nil {
		if drainErr := joinNC.Drain(); drainErr != nil {
			logger.Warn("failed to drain join NATS connection", "error", drainErr)
		}
		return fmt.Errorf("join request failed: %w", err)
	}

	var resp types.JoinResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		if drainErr := joinNC.Drain(); drainErr != nil {
			logger.Warn("failed to drain join NATS connection", "error", drainErr)
		}
		return fmt.Errorf("parsing join response: %w", err)
	}

	// Close the join NATS connection now that the handshake is complete.
	// The sidecar will create its own connection for ongoing communication.
	if err := joinNC.Drain(); err != nil {
		logger.Warn("failed to drain join NATS connection", "error", err)
	}

	if !resp.Accepted {
		return fmt.Errorf("join rejected: %s", resp.Error)
	}

	logger.Info("joined cluster successfully",
		"node_id", resp.NodeID,
		"tier", resp.Tier,
	)

	// Now start the sidecar in library mode
	var rtArgs []string
	if cfg.RuntimeArgs != "" {
		rtArgs = strings.Split(cfg.RuntimeArgs, ",")
	}

	// Load capabilities and team from manifest if provided.
	var capabilities []sidecar.Capability
	var teamID string
	if cfg.ManifestPath != "" {
		manifest, err := loadManifest(cfg.ManifestPath)
		if err != nil {
			return fmt.Errorf("loading manifest: %w", err)
		}
		teamID = manifest.Metadata.Team
		capabilities = convertCapabilities(manifest.Spec.Capabilities)
		logger.Info("loaded manifest",
			"manifest", cfg.ManifestPath,
			"team", teamID,
			"capabilities", len(capabilities),
		)
	}

	sidecarCfg := sidecar.Config{
		AgentID:        cfg.AgentID,
		TeamID:         teamID,
		NATSUrl:        natsURL,
		NATSToken:      cfg.NATSToken,
		HTTPAddr:       cfg.HTTPAddr,
		Capabilities:   capabilities,
		RuntimeCmd:     cfg.RuntimeCmd,
		RuntimeArgs:    rtArgs,
		WorkspacePath:  cfg.WorkDir,
		HealthInterval: 30 * time.Second,
		Tier:           "native",
		Mode:           sidecar.ModeLibrary,
	}

	s := sidecar.New(sidecarCfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		return fmt.Errorf("starting sidecar: %w", err)
	}

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sigCh)

	sig := <-sigCh
	logger.Info("received signal, shutting down", "signal", sig)

	if err := s.Stop(); err != nil {
		logger.Error("error stopping sidecar", "error", err)
	}

	return nil
}

// collectHardwareInventory gathers system hardware information.
func collectHardwareInventory(logger *slog.Logger) types.NodeResources {
	var res types.NodeResources

	// CPU count
	res.CPUCount = runtime.NumCPU()

	// Memory: read from /proc/meminfo if available
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				var kb int64
				n, _ := fmt.Sscanf(line, "MemTotal: %d kB", &kb)
				if n == 0 {
					logger.Warn("failed to parse MemTotal from /proc/meminfo", "line", line)
				} else {
					res.MemoryTotal = kb * 1024
				}
				break
			}
		}
	}

	// KVM availability
	if _, err := os.Stat("/dev/kvm"); err == nil {
		res.KVMAvail = true
	}

	return res
}

func getHostname(logger *slog.Logger) string {
	h, err := os.Hostname()
	if err != nil {
		logger.Warn("failed to get hostname, using fallback", "error", err)
	}
	if h == "" {
		h = "unknown-" + fmt.Sprintf("%x", time.Now().UnixNano()%0xFFFF)
	}
	return h
}

// loadManifest reads and parses an agent manifest YAML file.
func loadManifest(path string) (*types.AgentManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var manifest types.AgentManifest
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &manifest, nil
}

// convertCapabilities converts manifest capabilities to sidecar capabilities.
func convertCapabilities(agentCaps []types.AgentCapability) []sidecar.Capability {
	caps := make([]sidecar.Capability, len(agentCaps))
	for i, ac := range agentCaps {
		caps[i] = sidecar.Capability{
			Name:        ac.Name,
			Description: ac.Description,
			Inputs:      convertParams(ac.Inputs),
			Outputs:     convertParams(ac.Outputs),
			Async:       ac.Async,
		}
	}
	return caps
}

func convertParams(params []types.CapabilityParam) []sidecar.CapabilityParam {
	if len(params) == 0 {
		return nil
	}
	out := make([]sidecar.CapabilityParam, len(params))
	for i, p := range params {
		out[i] = sidecar.CapabilityParam{
			Name:        p.Name,
			Type:        p.Type,
			Description: p.Description,
			Required:    p.IsRequired(),
		}
	}
	return out
}
