package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/brandonapol/saddle/internal/app"
)

// peekCmd prints the last lines of a task's terminal: what the MCP peek tool
// returns, for callers that run the binary, such as the mod's needs-you band
// (#168).
func peekCmd() *cobra.Command {
	var lines int
	cmd := &cobra.Command{
		Use:   "peek <task>",
		Short: "Print the last lines of a task's terminal",
		Args:  cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			return peek(cmd.OutOrStdout(), a, args[0], lines)
		}),
	}
	cmd.Flags().IntVarP(&lines, "lines", "n", 40, "how many lines to print")
	return cmd
}

func peek(w io.Writer, a *app.App, task string, lines int) error {
	out, err := a.Peek(task, lines)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, out)
	return err
}
