package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/hivehq/hive/internal/auth"
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

			if err := auth.ValidateRole(auth.Role(role)); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}

			store, err := newStoreOnly()
			if err != nil {
				return err
			}

			// Generate a random token.
			rawToken, err := generateUserToken()
			if err != nil {
				return fmt.Errorf("generating token: %w", err)
			}

			tokenHash := auth.HashToken(rawToken)

			user := &auth.User{
				ID:        userID,
				Role:      auth.Role(role),
				TokenHash: tokenHash,
			}

			if teams != "" {
				user.Teams = splitTrimmed(teams)
			}
			if agents != "" {
				user.Agents = splitTrimmed(agents)
			}

			if err := store.AddUser(user); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("User %s created with role %s\n", userID, role)
			fmt.Printf("Token: %s\n", rawToken)
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
			store, err := newStoreOnly()
			if err != nil {
				return err
			}

			users := store.AllUsers()

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

			store, err := newStoreOnly()
			if err != nil {
				return err
			}

			user := store.GetUser(userID)
			if user == nil {
				fmt.Fprintf(os.Stderr, "Error: user %q not found\n", userID)
				os.Exit(1)
			}

			if role != "" {
				if err := auth.ValidateRole(auth.Role(role)); err != nil {
					fmt.Fprintf(os.Stderr, "Error: %v\n", err)
					os.Exit(1)
				}
				user.Role = auth.Role(role)
			}

			if cmd.Flags().Changed("teams") {
				if teams == "" {
					user.Teams = nil
				} else {
					user.Teams = splitTrimmed(teams)
				}
			}

			if cmd.Flags().Changed("agents") {
				if agents == "" {
					user.Agents = nil
				} else {
					user.Agents = splitTrimmed(agents)
				}
			}

			if err := store.UpdateUser(user); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
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

			store, err := newStoreOnly()
			if err != nil {
				return err
			}

			if err := store.RemoveUser(userID); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("User %s revoked\n", userID)
			return nil
		},
	}
}

// generateUserToken creates a random 32-byte hex-encoded token prefixed with "hive-user-".
func generateUserToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random bytes: %w", err)
	}
	return "hive-user-" + hex.EncodeToString(b), nil
}

func usersRotateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rotate USER_ID",
		Short: "Rotate a user's API token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			userID := args[0]

			store, err := newStoreOnly()
			if err != nil {
				return err
			}

			user := store.GetUser(userID)
			if user == nil {
				fmt.Fprintf(os.Stderr, "Error: user %q not found\n", userID)
				os.Exit(1)
			}

			rawToken, err := generateUserToken()
			if err != nil {
				return fmt.Errorf("generating token: %w", err)
			}

			user.TokenHash = auth.HashToken(rawToken)

			if err := store.UpdateUser(user); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Token rotated for user %s\n", userID)
			fmt.Printf("Token: %s\n", rawToken)
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
