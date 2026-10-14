package cli

import (
	"github.com/brandonapol/saddle/internal/agent"
	"github.com/spf13/cobra"
)

func codexBridgeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "codex-bridge <run-dir>",
		Short:  "Headless Codex session that speaks Claude stream-json on stdin",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return agent.RunCodexBridge(args[0], cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}
