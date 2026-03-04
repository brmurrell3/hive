// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/brmurrell3/hive/internal/config"
	"github.com/spf13/cobra"
)

var (
	clusterRoot  string
	outputFormat string
	controlPlane string
	authUser     string
	authToken    string
	version      = "dev"
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "hivectl",
		Short: "CLI tool for managing Hive agent clusters",
	}

	rootCmd.PersistentFlags().StringVar(&clusterRoot, "cluster-root", ".", "Path to the cluster root directory")
	rootCmd.PersistentFlags().StringVar(&outputFormat, "output", "table", "Output format: table or json")
	rootCmd.PersistentFlags().StringVar(&controlPlane, "control-plane", "", "Remote control plane address (host:port)")
	rootCmd.PersistentFlags().StringVar(&authUser, "user", "", "Username for RBAC authentication")
	rootCmd.PersistentFlags().StringVar(&authToken, "token", "", "Authentication token for RBAC")

	rootCmd.PersistentPreRunE = func(cmd *cobra.Command, args []string) error {
		// If --cluster-root wasn't explicitly set, fall back to HIVE_CONFIG env var.
		if !cmd.Flags().Changed("cluster-root") {
			if v := os.Getenv("HIVE_CONFIG"); v != "" {
				clusterRoot = v
			}
		}
		// If --control-plane wasn't explicitly set, fall back to HIVE_CONTROL_PLANE env var.
		if !cmd.Flags().Changed("control-plane") {
			if v := os.Getenv("HIVE_CONTROL_PLANE"); v != "" {
				controlPlane = v
			}
		}
		// If --user wasn't explicitly set, fall back to HIVE_USER env var.
		if !cmd.Flags().Changed("user") {
			if v := os.Getenv("HIVE_USER"); v != "" {
				authUser = v
			}
		}
		// If --token wasn't explicitly set, fall back to HIVE_TOKEN env var.
		if !cmd.Flags().Changed("token") {
			if v := os.Getenv("HIVE_TOKEN"); v != "" {
				authToken = v
			}
		}
		return nil
	}

	rootCmd.AddCommand(versionCmd())
	rootCmd.AddCommand(validateCmd())
	rootCmd.AddCommand(initCmd())
	rootCmd.AddCommand(agentsCmd())
	rootCmd.AddCommand(tokensCmd())
	rootCmd.AddCommand(nodesCmd())
	rootCmd.AddCommand(usersCmd())
	rootCmd.AddCommand(capabilitiesCmd())
	rootCmd.AddCommand(statusCmd())
	rootCmd.AddCommand(dashboardCmd())
	rootCmd.AddCommand(completionCmd())
	rootCmd.AddCommand(devCmd())
	rootCmd.AddCommand(triggerCmd())

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version of hivectl",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("hivectl %s\n", version)
		},
	}
}

func validateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate cluster configuration and agent manifests",
		Long: `Parse and validate the cluster root's cluster.yaml and all agent manifests
under agents/, reporting any schema errors or constraint violations without
contacting hived. Use this before committing changes or running hived.

Examples:
  # Validate the current directory
  hivectl validate
  # Output: Validation passed.

  # Validate a specific cluster root
  hivectl validate --cluster-root ./my-cluster
  # Output: Validation passed.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			absRoot, err := filepath.Abs(clusterRoot)
			if err != nil {
				return fmt.Errorf("resolving cluster root: %w", err)
			}

			ds, err := config.LoadDesiredState(absRoot)
			if err != nil {
				return fmt.Errorf("loading desired state: %w", err)
			}

			if err := config.ValidateDesiredState(ds); err != nil {
				return fmt.Errorf("validating desired state: %w", err)
			}

			fmt.Println("Validation passed.")
			return nil
		},
	}
}
