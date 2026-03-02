// SPDX-License-Identifier: Apache-2.0
// Copyright 2025 The Hive Authors

package main

import "github.com/spf13/cobra"

func completionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "completion [bash|zsh|fish]",
		Short: "Generate shell completion script",
		Long: `Generate shell completion script for hivectl.

To load completions:

  bash:  source <(hivectl completion bash)
  zsh:   hivectl completion zsh > "${fpath[1]}/_hivectl"
  fish:  hivectl completion fish | source`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish"},
		RunE: func(cmd *cobra.Command, args []string) error {
			switch args[0] {
			case "bash":
				return cmd.Root().GenBashCompletion(cmd.OutOrStdout())
			case "zsh":
				return cmd.Root().GenZshCompletion(cmd.OutOrStdout())
			case "fish":
				return cmd.Root().GenFishCompletion(cmd.OutOrStdout(), true)
			default:
				return cmd.Usage()
			}
		},
	}
}
