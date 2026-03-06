package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/brmurrell3/hive/internal/config"
	"github.com/brmurrell3/hive/internal/firmware"
	"github.com/spf13/cobra"
)

func firmwareCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "firmware",
		Short: "Manage firmware agents",
	}

	cmd.AddCommand(firmwareBuildCmd())
	cmd.AddCommand(firmwareFlashCmd())
	cmd.AddCommand(firmwareMonitorCmd())
	cmd.AddCommand(firmwareUpdateCmd())
	cmd.AddCommand(firmwareSignCmd())

	return cmd
}

func firmwareBuildCmd() *cobra.Command {
	var target string

	cmd := &cobra.Command{
		Use:   "build AGENT_ID",
		Short: "Build firmware for an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

			absRoot, err := filepath.Abs(clusterRoot)
			if err != nil {
				return fmt.Errorf("resolving cluster root: %w", err)
			}

			agents, err := config.LoadAgents(absRoot)
			if err != nil {
				return fmt.Errorf("loading agents: %w", err)
			}

			agent, ok := agents[agentID]
			if !ok {
				return fmt.Errorf("agent %q not found in manifests", agentID)
			}

			if agent.Spec.Firmware.Platform == "" {
				return fmt.Errorf("agent %q has no firmware platform specified", agentID)
			}

			platform := firmware.Platform(agent.Spec.Firmware.Platform)
			if target != "" {
				platform = firmware.Platform(target)
			}

			result, err := firmware.Build(firmware.BuildConfig{
				AgentID:     agentID,
				Platform:    platform,
				Board:       agent.Spec.Firmware.Board,
				ClusterRoot: absRoot,
				Logger:      logger,
			})
			if err != nil {
				return fmt.Errorf("building firmware: %w", err)
			}

			fmt.Printf("Firmware built successfully\n")
			fmt.Printf("  Binary: %s\n", result.BinaryPath)
			fmt.Printf("  Size: %d bytes\n", result.Size)
			return nil
		},
	}

	cmd.Flags().StringVar(&target, "target", "", "Override build target platform")

	return cmd
}

func firmwareFlashCmd() *cobra.Command {
	var (
		port       string
		baud       int
		binaryPath string
	)

	cmd := &cobra.Command{
		Use:   "flash AGENT_ID",
		Short: "Flash firmware to a connected device",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

			absRoot, err := filepath.Abs(clusterRoot)
			if err != nil {
				return fmt.Errorf("resolving cluster root: %w", err)
			}

			agents, err := config.LoadAgents(absRoot)
			if err != nil {
				return fmt.Errorf("loading agents: %w", err)
			}

			agent, ok := agents[agentID]
			if !ok {
				return fmt.Errorf("agent %q not found in manifests", agentID)
			}

			platform := firmware.Platform(agent.Spec.Firmware.Platform)

			// If --binary is not provided, derive the path from the platform's
			// default output location under the agent's build directory.
			if binaryPath == "" {
				outputDir := filepath.Join(absRoot, "agents", agentID, "firmware", "build")
				binaryPath, err = firmware.DefaultBinaryPath(platform, outputDir)
				if err != nil {
					return fmt.Errorf("resolving firmware binary path (hint: use --binary to specify explicitly): %w", err)
				}
			}

			if err := firmware.Flash(firmware.FlashConfig{
				AgentID:    agentID,
				Platform:   platform,
				BinaryPath: binaryPath,
				Port:       port,
				Baud:       baud,
				Logger:     logger,
			}); err != nil {
				return fmt.Errorf("flashing firmware: %w", err)
			}

			fmt.Printf("Firmware flashed to %s\n", port)
			return nil
		},
	}

	cmd.Flags().StringVar(&port, "port", "", "Serial port (e.g., /dev/ttyUSB0)")
	cmd.Flags().IntVar(&baud, "baud", 460800, "Flash baud rate")
	cmd.Flags().StringVar(&binaryPath, "binary", "", "Path to firmware binary (overrides auto-detected path)")

	return cmd
}

func firmwareMonitorCmd() *cobra.Command {
	var (
		port string
		baud int
	)

	cmd := &cobra.Command{
		Use:   "monitor AGENT_ID",
		Short: "Open serial monitor for firmware debugging",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

			absRoot, err := filepath.Abs(clusterRoot)
			if err != nil {
				return fmt.Errorf("resolving cluster root: %w", err)
			}

			agents, err := config.LoadAgents(absRoot)
			if err != nil {
				return fmt.Errorf("loading agents: %w", err)
			}

			agent, ok := agents[agentID]
			if !ok {
				return fmt.Errorf("agent %q not found in manifests", agentID)
			}

			platform := firmware.Platform(agent.Spec.Firmware.Platform)

			if err := firmware.Monitor(firmware.MonitorConfig{
				AgentID:  agentID,
				Port:     port,
				Baud:     baud,
				Platform: platform,
				Logger:   logger,
			}); err != nil {
				return fmt.Errorf("monitoring firmware: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&port, "port", "", "Serial port (e.g., /dev/ttyUSB0)")
	cmd.Flags().IntVar(&baud, "baud", 115200, "Monitor baud rate")

	return cmd
}

func firmwareUpdateCmd() *cobra.Command {
	var (
		binaryPath string
		version    string
		chunkSize  int
	)

	cmd := &cobra.Command{
		Use:   "update AGENT_ID",
		Short: "Initiate an OTA firmware update for an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]
			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

			absRoot, err := filepath.Abs(clusterRoot)
			if err != nil {
				return fmt.Errorf("resolving cluster root: %w", err)
			}

			agents, err := config.LoadAgents(absRoot)
			if err != nil {
				return fmt.Errorf("loading agents: %w", err)
			}

			if _, ok := agents[agentID]; !ok {
				return fmt.Errorf("agent %q not found in manifests", agentID)
			}

			// Verify the binary file exists before connecting to NATS.
			if _, err := os.Stat(binaryPath); err != nil {
				return fmt.Errorf("firmware binary %q: %w", binaryPath, err)
			}

			// Connect to hived's NATS server.
			nc, err := connectNATS("hivectl-firmware-update")
			if err != nil {
				return err
			}
			defer func() {
				_ = nc.Drain()
			}()

			// Use the OTA updater to push the firmware.
			updater := firmware.NewUpdater(nc, logger)

			// Handle Ctrl+C for graceful cancellation.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, os.Interrupt)
			go func() {
				<-sigCh
				fmt.Fprintf(os.Stderr, "\nCancelling OTA update...\n")
				cancel()
			}()

			fmt.Printf("Starting OTA update for agent %s from %s\n", agentID, binaryPath)

			update := firmware.Update{
				AgentID:         agentID,
				BinaryPath:      binaryPath,
				FirmwareVersion: version,
				ChunkSize:       chunkSize,
			}

			status, err := updater.Push(ctx, update)
			if err != nil {
				return fmt.Errorf("OTA update failed: %w", err)
			}

			if status.Status == firmware.StatusComplete {
				fmt.Printf("OTA update completed successfully for agent %s\n", agentID)
			} else {
				return fmt.Errorf("OTA update finished with status %q: %s", status.Status, status.Error)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&binaryPath, "binary", "", "Path to firmware binary (required)")
	cmd.Flags().StringVar(&version, "version", "0.0.0", "Firmware version string")
	cmd.Flags().IntVar(&chunkSize, "chunk-size", firmware.DefaultChunkSize, "OTA chunk size in bytes")
	_ = cmd.MarkFlagRequired("binary")

	return cmd
}

func firmwareSignCmd() *cobra.Command {
	var keyPath string

	cmd := &cobra.Command{
		Use:   "sign AGENT_ID",
		Short: "Sign firmware for an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_ = args[0]
			_ = keyPath

			return fmt.Errorf("firmware signing is not yet implemented: requires key management infrastructure (PKI) which has not been built")
		},
	}

	cmd.Flags().StringVar(&keyPath, "key", "", "Path to signing key (required)")
	_ = cmd.MarkFlagRequired("key")

	return cmd
}
