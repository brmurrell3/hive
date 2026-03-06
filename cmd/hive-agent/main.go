package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/brmurrell3/hive/internal/sidecar"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

var version = "dev"

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
	)

	cmd := &cobra.Command{
		Use:   "join",
		Short: "Join a Hive cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
				Level: slog.LevelInfo,
			}))

			if token == "" {
				return fmt.Errorf("--token is required")
			}
			if controlPlane == "" {
				return fmt.Errorf("--control-plane is required")
			}
			if agentID == "" {
				return fmt.Errorf("--agent-id is required")
			}

			return runJoin(logger, token, controlPlane, agentID, runtimeCmd, runtimeArgs, workDir, httpAddr, natsToken, manifestPath)
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

	return cmd
}

func runJoin(logger *slog.Logger, token, controlPlane, agentID, runtimeCmd, runtimeArgs, workDir, httpAddr, natsToken, manifestPath string) error {
	logger.Info("joining cluster",
		"control_plane", controlPlane,
		"agent_id", agentID,
	)

	// Build NATS URL from control plane address
	natsURL := fmt.Sprintf("nats://%s", controlPlane)

	// Connect to NATS first to send the join request
	natsOpts := []nats.Option{
		nats.Name(fmt.Sprintf("hive-agent-%s", agentID)),
		nats.ReconnectWait(2 * time.Second),
		nats.MaxReconnects(-1),
	}
	if natsToken != "" {
		natsOpts = append(natsOpts, nats.Token(natsToken))
	}

	nc, err := nats.Connect(natsURL, natsOpts...)
	if err != nil {
		return fmt.Errorf("connecting to NATS at %s: %w", natsURL, err)
	}
	defer nc.Close()

	// Collect hardware inventory
	inventory := collectHardwareInventory()

	// Send join request
	joinReq := types.JoinRequest{
		Token:     token,
		Hostname:  getHostname(),
		Arch:      runtime.GOARCH,
		Resources: inventory,
		AgentID:   agentID,
	}

	reqData, err := json.Marshal(joinReq)
	if err != nil {
		return fmt.Errorf("marshaling join request: %w", err)
	}

	// Send request-reply
	msg, err := nc.Request("hive.join.request", reqData, 10*time.Second)
	if err != nil {
		return fmt.Errorf("join request failed: %w", err)
	}

	var resp types.JoinResponse
	if err := json.Unmarshal(msg.Data, &resp); err != nil {
		return fmt.Errorf("parsing join response: %w", err)
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
	if runtimeArgs != "" {
		rtArgs = strings.Split(runtimeArgs, ",")
	}

	// Load capabilities and team from manifest if provided.
	var capabilities []sidecar.Capability
	var teamID string
	if manifestPath != "" {
		manifest, err := loadManifest(manifestPath)
		if err != nil {
			return fmt.Errorf("loading manifest: %w", err)
		}
		teamID = manifest.Metadata.Team
		capabilities = convertCapabilities(manifest.Spec.Capabilities)
		logger.Info("loaded manifest",
			"manifest", manifestPath,
			"team", teamID,
			"capabilities", len(capabilities),
		)
	}

	cfg := sidecar.Config{
		AgentID:        agentID,
		TeamID:         teamID,
		NATSUrl:        natsURL,
		NATSToken:      natsToken,
		HTTPAddr:       httpAddr,
		Capabilities:   capabilities,
		RuntimeCmd:     runtimeCmd,
		RuntimeArgs:    rtArgs,
		WorkspacePath:  workDir,
		HealthInterval: 30 * time.Second,
		Tier:           "native",
		Mode:           sidecar.ModeLibrary,
	}

	s := sidecar.New(cfg, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := s.Start(ctx); err != nil {
		return fmt.Errorf("starting sidecar: %w", err)
	}

	// Wait for signal
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	sig := <-sigCh
	logger.Info("received signal, shutting down", "signal", sig)

	if err := s.Stop(); err != nil {
		logger.Error("error stopping sidecar", "error", err)
	}

	// Close the separate NATS connection used for join
	nc.Close()

	return nil
}

// collectHardwareInventory gathers system hardware information.
func collectHardwareInventory() types.NodeResources {
	var res types.NodeResources

	// CPU count
	res.CPUCount = runtime.NumCPU()

	// Memory: read from /proc/meminfo if available
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				var kb int64
				fmt.Sscanf(line, "MemTotal: %d kB", &kb)
				res.MemoryTotal = kb * 1024
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

func getHostname() string {
	h, _ := os.Hostname()
	if h == "" {
		h = "unknown"
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
