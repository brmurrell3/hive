// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/brmurrell3/hive/internal/auth"
	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/spf13/cobra"
)

func usersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "users",
		Short: "Manage RBAC users",
	}

	cmd.AddCommand(usersCreateCmd())
	cmd.AddCommand(usersListCmd())
	cmd.AddCommand(usersUpdateCmd())
	cmd.AddCommand(usersRevokeCmd())
	cmd.AddCommand(usersRotateCmd())

	return cmd
}

func usersCreateCmd() *cobra.Command {
	var (
		role   string
		teams  string
		agents string
	)

	cmd := &cobra.Command{
		Use:   "create USER_ID",
		Short: "Create a new user with an API token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			userID := args[0]

			if err := types.ValidateSubjectComponent("user_id", userID); err != nil {
				return err
			}

			if err := auth.ValidateRole(auth.Role(role)); err != nil {
				return fmt.Errorf("validating role: %w", err)
			}

			client, err := newDaemonClient()
			if err != nil {
				return err
			}
			defer client.Close()

			payload := map[string]interface{}{
				"user_id": userID,
				"role":    role,
			}
			if teams != "" {
				payload["teams"] = splitTrimmed(teams)
			}
			if agents != "" {
				payload["agents"] = splitTrimmed(agents)
			}

			resp, err := client.request(protocol.SubjUserCreate, payload)
			if err != nil {
				return fmt.Errorf("creating user: %w", err)
			}
			if err := resp.Err(); err != nil {
				return err
			}

			var result struct {
				UserID string `json:"user_id"`
				Role   string `json:"role"`
				Token  string `json:"token"`
			}
			if err := json.Unmarshal(resp.Data, &result); err != nil {
				return fmt.Errorf("parsing response: %w", err)
			}

			fmt.Printf("User %s created with role %s\n", result.UserID, result.Role)
			fmt.Printf("Token: %s\n", result.Token)
			fmt.Println("Save this token - it cannot be retrieved later.")
			return nil
		},
	}

	cmd.Flags().StringVar(&role, "role", "", "User role: admin, operator, or viewer (required)")
	cmd.Flags().StringVar(&teams, "teams", "", "Comma-separated list of team IDs the user can access")
	cmd.Flags().StringVar(&agents, "agents", "", "Comma-separated list of agent IDs the user can access")
	_ = cmd.MarkFlagRequired("role")

	return cmd
}

func usersListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List all users",
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newDaemonClient()
			if err != nil {
				return err
			}
			defer client.Close()

			resp, err := client.request(protocol.SubjUserList, nil)
			if err != nil {
				return fmt.Errorf("listing users: %w", err)
			}
			if err := resp.Err(); err != nil {
				return err
			}

			var users []*auth.User
			if err := json.Unmarshal(resp.Data, &users); err != nil {
				return fmt.Errorf("parsing response: %w", err)
			}

			w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "USER_ID\tROLE\tTEAMS\tAGENTS")

			for _, u := range users {
				teams := "-"
				if len(u.Teams) > 0 {
					teams = strings.Join(u.Teams, ",")
				}
				agents := "-"
				if len(u.Agents) > 0 {
					agents = strings.Join(u.Agents, ",")
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", u.ID, u.Role, teams, agents)
			}

			w.Flush()
			return nil
		},
	}
}

func usersUpdateCmd() *cobra.Command {
	var (
		role   string
		teams  string
		agents string
	)

	cmd := &cobra.Command{
		Use:   "update USER_ID",
		Short: "Update a user's role or access scope",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			userID := args[0]

			if err := types.ValidateSubjectComponent("user_id", userID); err != nil {
				return err
			}

			if role != "" {
				if err := auth.ValidateRole(auth.Role(role)); err != nil {
					return fmt.Errorf("validating role: %w", err)
				}
			}

			client, err := newDaemonClient()
			if err != nil {
				return err
			}
			defer client.Close()

			payload := map[string]interface{}{
				"user_id": userID,
			}
			if role != "" {
				payload["role"] = role
			}

			if cmd.Flags().Changed("teams") {
				if teams == "" {
					payload["clear_teams"] = true
				} else {
					payload["teams"] = splitTrimmed(teams)
				}
			}

			if cmd.Flags().Changed("agents") {
				if agents == "" {
					payload["clear_agents"] = true
				} else {
					payload["agents"] = splitTrimmed(agents)
				}
			}

			resp, err := client.request(protocol.SubjUserUpdate, payload)
			if err != nil {
				return fmt.Errorf("updating user: %w", err)
			}
			if err := resp.Err(); err != nil {
				return err
			}

			fmt.Printf("User %s updated\n", userID)
			return nil
		},
	}

	cmd.Flags().StringVar(&role, "role", "", "New role: admin, operator, or viewer")
	cmd.Flags().StringVar(&teams, "teams", "", "New comma-separated list of team IDs")
	cmd.Flags().StringVar(&agents, "agents", "", "New comma-separated list of agent IDs")

	return cmd
}

func usersRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke USER_ID",
		Short: "Revoke a user (remove from RBAC)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			userID := args[0]

			if err := types.ValidateSubjectComponent("user_id", userID); err != nil {
				return err
			}

			client, err := newDaemonClient()
			if err != nil {
				return err
			}
			defer client.Close()

			resp, err := client.request(protocol.SubjUserRevoke, protocol.CtlRequest{AgentID: userID})
			if err != nil {
				return fmt.Errorf("revoking user: %w", err)
			}
			if err := resp.Err(); err != nil {
				return err
			}

			fmt.Printf("User %s revoked\n", userID)
			return nil
		},
	}
}

func usersRotateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rotate USER_ID",
		Short: "Rotate a user's API token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			userID := args[0]

			if err := types.ValidateSubjectComponent("user_id", userID); err != nil {
				return err
			}

			client, err := newDaemonClient()
			if err != nil {
				return err
			}
			defer client.Close()

			resp, err := client.request(protocol.SubjUserRotate, protocol.CtlRequest{AgentID: userID})
			if err != nil {
				return fmt.Errorf("rotating user token: %w", err)
			}
			if err := resp.Err(); err != nil {
				return err
			}

			var result struct {
				Token string `json:"token"`
			}
			if err := json.Unmarshal(resp.Data, &result); err != nil {
				return fmt.Errorf("parsing response: %w", err)
			}

			fmt.Printf("Token rotated for user %s\n", userID)
			fmt.Printf("Token: %s\n", result.Token)
			fmt.Println("Save this token - it cannot be retrieved later.")
			return nil
		},
	}
}

// splitTrimmed splits a comma-separated string and trims whitespace.
func splitTrimmed(s string) []string {
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
