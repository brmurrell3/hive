// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/brmurrell3/hive/internal/config"
	hivenats "github.com/brmurrell3/hive/internal/nats"
	"github.com/brmurrell3/hive/internal/sidecar"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/fsnotify/fsnotify"
	"github.com/spf13/cobra"
)

var (
	devSidecarPortBase  int
	devCallbackPortBase int
)

func devCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dev",
		Short: "Start a local development environment",
		Long: `Start all agents from the cluster root in local development mode.
Uses the process backend for all agents regardless of tier settings.
Logs are human-readable at debug level. Agents restart on manifest changes.

Examples:
  hivectl dev --cluster-root ./demo
  hivectl dev --cluster-root ./demo`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDev(cmd.Context())
		},
	}

	cmd.Flags().IntVar(&devSidecarPortBase, "sidecar-port-base", 9100, "Base port for sidecar HTTP servers")
	cmd.Flags().IntVar(&devCallbackPortBase, "callback-port-base", 9200, "Base port for agent callback servers")

	return cmd
}

// devAgent holds the state for a running agent in dev mode.
type devAgent struct {
	id          string
	teamID      string
	index       int // position in sorted agent list (determines ports)
	sidecar     *sidecar.Sidecar
	manifest    *types.AgentManifest
	envVars     []string
	callbackURL string
	sidecarURL  string
}

func runDev(ctx context.Context) error {
	absRoot, err := filepath.Abs(clusterRoot)
	if err != nil {
		return fmt.Errorf("resolving cluster root: %w", err)
	}

	// Verify cluster root exists.
	if _, err := os.Stat(filepath.Join(absRoot, "cluster.yaml")); err != nil {
		return fmt.Errorf("invalid cluster root %q: cluster.yaml not found", absRoot)
	}

	// Load and validate manifests.
	ds, err := config.LoadDesiredState(absRoot)
	if err != nil {
		return fmt.Errorf("loading manifests: %w", err)
	}
	if err := config.ValidateDesiredState(ds); err != nil {
		return fmt.Errorf("invalid manifests: %w", err)
	}

	// Setup logger (human-readable text at debug level).
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelDebug,
	}))

	logger.Info("starting dev environment",
		"cluster_root", absRoot,
		"agents", len(ds.Agents),
	)

	// Start embedded NATS. Disable clustering for dev mode.
	natsCfg := ds.Cluster.Spec.NATS
	if natsCfg.Port == 0 {
		natsCfg.Port = 4222
	}
	natsCfg.ClusterPort = -1 // disable clustering in dev mode

	dataDir := filepath.Join(absRoot, ".state")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return fmt.Errorf("creating state directory: %w", err)
	}

	ns, err := hivenats.NewServer(natsCfg, dataDir, logger)
	if err != nil {
		return fmt.Errorf("creating NATS server: %w", err)
	}
	if err := ns.Start(); err != nil {
		return fmt.Errorf("starting NATS server: %w", err)
	}
	defer ns.Shutdown()

	natsURL := ns.ClientURL()
	natsToken := ns.AuthToken()
	logger.Info("NATS server started", "url", natsURL)

	// Write NATS connection info so hivectl trigger can connect.
	connInfo := fmt.Sprintf("HIVE_NATS_URL=%s\nHIVE_NATS_TOKEN=%s\n", natsURL, natsToken)
	connInfoPath := filepath.Join(dataDir, "nats.env")
	if err := os.WriteFile(connInfoPath, []byte(connInfo), 0600); err != nil {
		logger.Warn("failed to write NATS connection info", "error", err)
	}
	defer os.Remove(connInfoPath)

	// Build set of lead agents from team manifests.
	leadAgents := make(map[string]bool)
	for _, team := range ds.Teams {
		if team.Spec.Lead != "" {
			leadAgents[team.Spec.Lead] = true
		}
	}

	// Sort agents by ID so port assignment is deterministic.
	agentList := make([]*types.AgentManifest, 0, len(ds.Agents))
	for _, a := range ds.Agents {
		agentList = append(agentList, a)
	}
	sort.Slice(agentList, func(i, j int) bool {
		return agentList[i].Metadata.ID < agentList[j].Metadata.ID
	})

	// Create and start a sidecar for each agent.
	var mu sync.Mutex
	agents := make(map[string]*devAgent)

	for i, agent := range agentList {
		da, err := startDevAgent(ctx, agent, i, absRoot, natsURL, natsToken, leadAgents, logger)
		if err != nil {
			// Stop already-started agents.
			for _, a := range agents {
				a.sidecar.Stop() //nolint:errcheck
			}
			return fmt.Errorf("starting agent %q: %w", agent.Metadata.ID, err)
		}
		agents[agent.Metadata.ID] = da
	}

	logger.Info("all agents started, press Ctrl+C to stop",
		"agents", len(agents),
	)

	// Start file watcher for manifest hot-reload.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		logger.Warn("failed to create file watcher, hot-reload disabled", "error", err)
	} else {
		defer watcher.Close() //nolint:errcheck

		// Watch each agent's manifest directory.
		for _, agent := range agentList {
			agentDir := filepath.Join(absRoot, "agents", agent.Metadata.ID)
			if err := watcher.Add(agentDir); err != nil {
				logger.Warn("failed to watch agent directory", "agent_id", agent.Metadata.ID, "error", err)
			}
		}

		// Debounce timers per agent.
		debounce := make(map[string]*time.Timer)

		go func() {
			for {
				select {
				case event, ok := <-watcher.Events:
					if !ok {
						return
					}
					// Only react to manifest.yaml writes.
					if filepath.Base(event.Name) != "manifest.yaml" {
						continue
					}
					if event.Op&(fsnotify.Write|fsnotify.Create) == 0 {
						continue
					}

					// Extract agent ID from path.
					agentID := filepath.Base(filepath.Dir(event.Name))

					mu.Lock()
					// Cancel any pending debounce for this agent.
					if t, ok := debounce[agentID]; ok {
						t.Stop()
					}
					debounce[agentID] = time.AfterFunc(500*time.Millisecond, func() {
						mu.Lock()
						da, exists := agents[agentID]
						mu.Unlock()
						if !exists {
							return
						}
						restartDevAgent(ctx, da, absRoot, natsURL, natsToken, leadAgents, &mu, agents, logger)
					})
					mu.Unlock()

				case err, ok := <-watcher.Errors:
					if !ok {
						return
					}
					logger.Warn("file watcher error", "error", err)
				}
			}
		}()

		logger.Info("hot-reload enabled, watching for manifest changes")
	}

	// Wait for interrupt signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	logger.Info("shutting down...")

	// Stop all sidecars in reverse order.
	mu.Lock()
	agentIDs := make([]string, 0, len(agents))
	for id := range agents {
		agentIDs = append(agentIDs, id)
	}
	mu.Unlock()
	sort.Sort(sort.Reverse(sort.StringSlice(agentIDs)))
	for _, id := range agentIDs {
		mu.Lock()
		da := agents[id]
		mu.Unlock()
		if err := da.sidecar.Stop(); err != nil {
			logger.Error("error stopping agent", "agent_id", id, "error", err)
		}
	}

	logger.Info("dev environment stopped")
	return nil
}

// startDevAgent creates and starts a sidecar for a single agent.
func startDevAgent(ctx context.Context, agent *types.AgentManifest, index int, absRoot, natsURL, natsToken string, leadAgents map[string]bool, logger *slog.Logger) (*devAgent, error) {
	agentID := agent.Metadata.ID
	teamID := agent.Metadata.Team

	callbackPort := devCallbackPortBase + index
	sidecarPort := devSidecarPortBase + index
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d", callbackPort)
	sidecarURL := fmt.Sprintf("http://127.0.0.1:%d", sidecarPort)

	// Build capabilities from manifest.
	caps := make([]sidecar.Capability, 0, len(agent.Spec.Capabilities))
	for _, c := range agent.Spec.Capabilities {
		cap := sidecar.Capability{
			Name:        c.Name,
			Description: c.Description,
		}
		for _, inp := range c.Inputs {
			cap.Inputs = append(cap.Inputs, sidecar.CapabilityParam{
				Name:        inp.Name,
				Type:        inp.Type,
				Description: inp.Description,
				Required:    inp.IsRequired(),
			})
		}
		for _, out := range c.Outputs {
			cap.Outputs = append(cap.Outputs, sidecar.CapabilityParam{
				Name:        out.Name,
				Type:        out.Type,
				Description: out.Description,
			})
		}
		caps = append(caps, cap)
	}

	runtimeCmd := agent.Spec.Runtime.Command
	agentDir := filepath.Join(absRoot, "agents", agentID)

	envVars := []string{
		fmt.Sprintf("HIVE_AGENT_ID=%s", agentID),
		fmt.Sprintf("HIVE_TEAM_ID=%s", teamID),
		fmt.Sprintf("HIVE_TEAM=%s", teamID),
		fmt.Sprintf("HIVE_NATS_URL=%s", natsURL),
		fmt.Sprintf("HIVE_SIDECAR_URL=%s", sidecarURL),
		fmt.Sprintf("HIVE_CALLBACK_PORT=%d", callbackPort),
		fmt.Sprintf("HIVE_WORKSPACE=%s", agentDir),
	}
	if natsToken != "" {
		envVars = append(envVars, fmt.Sprintf("HIVE_NATS_TOKEN=%s", natsToken))
	}
	for k, v := range agent.Spec.Runtime.Model.Env {
		envVars = append(envVars, fmt.Sprintf("%s=%s", k, v))
	}

	cfg := sidecar.Config{
		AgentID:       agentID,
		TeamID:        teamID,
		NATSUrl:       natsURL,
		NATSToken:     natsToken,
		HTTPAddr:      fmt.Sprintf(":%d", sidecarPort),
		Capabilities:  caps,
		RuntimeCmd:    runtimeCmd,
		WorkspacePath: agentDir,
		Tier:          "native",
		Mode:          sidecar.ModeLibrary,
		CallbackURL:   callbackURL,
		IsLead:        leadAgents[agentID],
	}

	sc := sidecar.New(cfg, logger)
	sc.SetEnvVars(envVars)

	logger.Info("starting agent",
		"agent_id", agentID,
		"team", teamID,
		"sidecar_port", sidecarPort,
		"callback_port", callbackPort,
	)

	if err := sc.Start(ctx); err != nil {
		return nil, err
	}

	return &devAgent{
		id:          agentID,
		teamID:      teamID,
		index:       index,
		sidecar:     sc,
		manifest:    agent,
		envVars:     envVars,
		callbackURL: callbackURL,
		sidecarURL:  sidecarURL,
	}, nil
}

// restartDevAgent stops and restarts a single agent after a manifest change.
func restartDevAgent(ctx context.Context, da *devAgent, absRoot, natsURL, natsToken string, leadAgents map[string]bool, mu *sync.Mutex, agents map[string]*devAgent, logger *slog.Logger) {
	logger.Info("manifest changed, restarting agent", "agent_id", da.id)

	// Re-load and validate the manifest.
	ds, err := config.LoadDesiredState(absRoot)
	if err != nil {
		logger.Error("failed to reload manifests", "error", err)
		return
	}
	if err := config.ValidateDesiredState(ds); err != nil {
		logger.Error("invalid manifests after change", "agent_id", da.id, "error", err)
		return
	}

	newManifest, ok := ds.Agents[da.id]
	if !ok {
		logger.Warn("agent no longer in manifests after reload", "agent_id", da.id)
		return
	}

	// Stop the old sidecar.
	if err := da.sidecar.Stop(); err != nil {
		logger.Error("error stopping agent for restart", "agent_id", da.id, "error", err)
	}

	// Pause for port release. The old runtime process may take a moment to
	// fully release its listening socket after SIGTERM.
	time.Sleep(1 * time.Second)

	// Start a new sidecar with the updated manifest.
	newDA, err := startDevAgent(ctx, newManifest, da.index, absRoot, natsURL, natsToken, leadAgents, logger)
	if err != nil {
		logger.Error("failed to restart agent", "agent_id", da.id, "error", err)
		return
	}

	mu.Lock()
	agents[da.id] = newDA
	mu.Unlock()

	logger.Info("agent restarted successfully", "agent_id", da.id)
}
