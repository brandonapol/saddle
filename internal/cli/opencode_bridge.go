package cli

import (
	"github.com/brandonapol/saddle/internal/agent"
	"github.com/spf13/cobra"
)

func openCodeBridgeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "opencode-bridge <run-dir>",
		Short:  "Headless opencode session that speaks Claude stream-json on stdin",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return agent.RunOpenCodeBridge(args[0], cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}
