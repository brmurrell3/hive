package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/hivehq/hive/internal/state"
	"github.com/hivehq/hive/internal/token"
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
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := newStoreOnly()
			if err != nil {
				return err
			}

			var ttlDuration time.Duration
			if ttl != "" {
				ttlDuration, err = time.ParseDuration(ttl)
				if err != nil {
					return fmt.Errorf("invalid TTL %q: %w", ttl, err)
				}
			}

			rawToken, err := token.Generate(store, ttlDuration)
			if err != nil {
				return fmt.Errorf("generating token: %w", err)
			}

			fmt.Println(rawToken)
			return nil
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
			store, err := newStoreOnly()
			if err != nil {
				return err
			}

			tokens := store.AllTokens()

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
		},
	}
}

func tokensRevokeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "revoke PREFIX",
		Short: "Revoke a token by its prefix",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, err := newStoreOnly()
			if err != nil {
				return err
			}

			prefix := args[0]
			if err := store.RevokeToken(prefix); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}

			fmt.Printf("Token %s revoked\n", prefix)
			return nil
		},
	}
}

func newStoreOnly() (*state.Store, error) {
	absRoot, err := filepath.Abs(clusterRoot)
	if err != nil {
		return nil, fmt.Errorf("resolving cluster root: %w", err)
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	statePath := filepath.Join(absRoot, "state.json")
	return state.NewStore(statePath, logger)
}
