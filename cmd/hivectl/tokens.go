// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/brmurrell3/hive/internal/protocol"
	"github.com/brmurrell3/hive/internal/types"
	"github.com/spf13/cobra"
)

func tokensCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tokens",
		Short: "Manage join tokens",
	}

	cmd.AddCommand(tokensCreateCmd())
	cmd.AddCommand(tokensListCmd())
	cmd.AddCommand(tokensRevokeCmd())

	return cmd
}

func tokensCreateCmd() *cobra.Command {
	var ttl string

	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create a new join token",
		Long: `Generate a new join token that Tier 2 native agents and sidecars use to
authenticate with hived. Tokens are stored hashed in state and can be
given an optional TTL after which they are automatically rejected.

Examples:
  # Create a token with no expiry
  hivectl tokens create
  # Output: hvtk_a1b2c3d4...

  # Create a token that expires in 48 hours
  hivectl tokens create --ttl 48h
  # Output: hvtk_e5f6g7h8...`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if ttl != "" {
				// Validate TTL format locally before sending.
				if _, err := time.ParseDuration(ttl); err != nil {
					return fmt.Errorf("invalid TTL %q: %w", ttl, err)
				}
			}

			return withClient(func(client *DaemonClient) error {
				payload := map[string]string{}
				if ttl != "" {
					payload["ttl"] = ttl
				}

				resp, err := client.request(protocol.SubjTokenCreate, payload)
				if err != nil {
					return fmt.Errorf("creating token: %w", err)
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

				fmt.Println(result.Token)
				return nil
			})
		},
	}

	cmd.Flags().StringVar(&ttl, "ttl", "", "Token time-to-live (e.g., 24h, 7d)")

	return cmd
}

func tokensListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List active tokens",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withClient(func(client *DaemonClient) error {
				resp, err := client.request(protocol.SubjTokenList, nil)
				if err != nil {
					return fmt.Errorf("listing tokens: %w", err)
				}
				if err := resp.Err(); err != nil {
					return err
				}

				var tokens []*types.Token
				if err := json.Unmarshal(resp.Data, &tokens); err != nil {
					return fmt.Errorf("parsing response: %w", err)
				}

				w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
				fmt.Fprintln(w, "PREFIX\tCREATED\tEXPIRES\tLAST USED\tSTATUS")

				for _, t := range tokens {
					expires := "-"
					if !t.ExpiresAt.IsZero() {
						expires = t.ExpiresAt.Format(time.RFC3339)
					}

					lastUsed := "-"
					if !t.LastUsed.IsZero() {
						lastUsed = t.LastUsed.Format(time.RFC3339)
					}

					status := "active"
					if t.Revoked {
						status = "revoked"
					} else if t.IsExpired() {
						status = "expired"
					}

					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
						t.Prefix,
						t.CreatedAt.Format(time.RFC3339),
						expires,
						lastUsed,
						status,
					)
				}

				w.Flush()
				return nil
			})
		},
	}
}

func tokensRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke PREFIX",
		Short: "Revoke a token by its prefix",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prefix := args[0]

			return withClient(func(client *DaemonClient) error {
				resp, err := client.request(protocol.SubjTokenRevoke, map[string]string{"prefix": prefix})
				if err != nil {
					return fmt.Errorf("revoking token: %w", err)
				}
				if err := resp.Err(); err != nil {
					return err
				}

				fmt.Printf("Token %s revoked\n", prefix)
				return nil
			})
		},
	}
}
