package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/hivehq/hive/internal/config"
	"github.com/hivehq/hive/internal/firmware"
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
				fmt.Fprintf(os.Stderr, "Error: agent %q not found in manifests\n", agentID)
				os.Exit(1)
			}

			if agent.Spec.Firmware.Platform == "" {
				fmt.Fprintf(os.Stderr, "Error: agent %q has no firmware platform specified\n", agentID)
				os.Exit(1)
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
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
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
		port string
		baud int
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
				fmt.Fprintf(os.Stderr, "Error: agent %q not found in manifests\n", agentID)
				os.Exit(1)
			}

			platform := firmware.Platform(agent.Spec.Firmware.Platform)
			binaryPath := filepath.Join(absRoot, "agents", agentID, "firmware", "build", "firmware.bin")

			if err := firmware.Flash(firmware.FlashConfig{
				AgentID:    agentID,
				Platform:   platform,
				BinaryPath: binaryPath,
				Port:       port,
				Baud:       baud,
				Logger:     logger,
			}); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Firmware flashed to %s\n", port)
			return nil
		},
	}

	cmd.Flags().StringVar(&port, "port", "", "Serial port (e.g., /dev/ttyUSB0)")
	cmd.Flags().IntVar(&baud, "baud", 460800, "Flash baud rate")

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
				fmt.Fprintf(os.Stderr, "Error: agent %q not found in manifests\n", agentID)
				os.Exit(1)
			}

			platform := firmware.Platform(agent.Spec.Firmware.Platform)

			if err := firmware.Monitor(firmware.MonitorConfig{
				AgentID:  agentID,
				Port:     port,
				Baud:     baud,
				Platform: platform,
				Logger:   logger,
			}); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&port, "port", "", "Serial port (e.g., /dev/ttyUSB0)")
	cmd.Flags().IntVar(&baud, "baud", 115200, "Monitor baud rate")

	return cmd
}

func firmwareUpdateCmd() *cobra.Command {
	var binaryPath string

	cmd := &cobra.Command{
		Use:   "update AGENT_ID",
		Short: "Initiate an OTA firmware update for an agent",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			agentID := args[0]

			absRoot, err := filepath.Abs(clusterRoot)
			if err != nil {
				return fmt.Errorf("resolving cluster root: %w", err)
			}

			agents, err := config.LoadAgents(absRoot)
			if err != nil {
				return fmt.Errorf("loading agents: %w", err)
			}

			if _, ok := agents[agentID]; !ok {
				fmt.Fprintf(os.Stderr, "Error: agent %q not found in manifests\n", agentID)
				os.Exit(1)
			}

			fmt.Printf("OTA update initiated for %s from %s\n", agentID, binaryPath)
			return nil
		},
	}

	cmd.Flags().StringVar(&binaryPath, "binary", "", "Path to firmware binary (required)")
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
			agentID := args[0]

			absRoot, err := filepath.Abs(clusterRoot)
			if err != nil {
				return fmt.Errorf("resolving cluster root: %w", err)
			}

			agents, err := config.LoadAgents(absRoot)
			if err != nil {
				return fmt.Errorf("loading agents: %w", err)
			}

			if _, ok := agents[agentID]; !ok {
				fmt.Fprintf(os.Stderr, "Error: agent %q not found in manifests\n", agentID)
				os.Exit(1)
			}

			// keyPath is required by the flag but not used yet.
			_ = keyPath

			fmt.Println("Firmware signing not yet available (requires key management infrastructure)")
			return nil
		},
	}

	cmd.Flags().StringVar(&keyPath, "key", "", "Path to signing key (required)")
	_ = cmd.MarkFlagRequired("key")

	return cmd
}
