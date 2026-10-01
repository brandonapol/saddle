// Package cli wires saddle's cobra commands.
package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// Version is set at build time with -ldflags "-X github.com/brandonapol/saddle/internal/cli.Version=...".
var Version = "dev"

var errNotImplemented = errors.New("not implemented yet")

func Root() *cobra.Command {
	root := &cobra.Command{
		Use:           "saddle",
		Short:         "Ride a herd of coding agents from one terminal",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.AddCommand(
		&cobra.Command{
			Use:   "version",
			Short: "Print the saddle version",
			Run:   func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), Version) },
		},
		stub("up", "Start saddled for this repo (#5)"),
		stub("down", "Stop saddled for this repo (#5)"),
		stub("daemon", "Run saddled in the foreground (#5)"),
		stub("plan [epic]", "Plan an epic into a task DAG: file, gh:#N, or - (#7)"),
		stub("run", "Launch the approved plan (#17)"),
		stub("status", "Show tasks, agents and the merge train (#5)"),
		stub("tui", "Open the control window (#33)"),
		stub("hook <event>", "Claude Code hook entrypoint (#21)"),
		stub("mcp", "Stdio MCP server for agents (#22)"),
		stub("brief <task>", "Live task brief pane (#37)"),
	)
	return root
}

func stub(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		RunE:  func(*cobra.Command, []string) error { return errNotImplemented },
	}
}
