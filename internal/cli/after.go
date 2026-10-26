package cli

import (
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/spf13/cobra"
)

func afterCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "after <task> <dep>...",
		Short: "Record that a task builds on other tasks' work",
		Long: `Records after edges once a task is spawned, in the same .saddle/deps file
spawn --after writes: task builds on each dep's work, so prs stacks its PR on
theirs even when their files don't overlap and no issue is shared. Use it when
the pre-publish gate says a layer fails for want of a sibling's commit; the
gate prints the exact command. Run prs again afterwards.`,
		Args: cobra.MinimumNArgs(2),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			added, err := a.After(args[0], args[1:])
			if err != nil {
				return err
			}
			if len(added) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "%s already runs after %s\n", args[0], strings.Join(args[1:], ", "))
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s now runs after %s; run prs to restack its PR\n", args[0], strings.Join(added, ", "))
			return nil
		}),
	}
}
