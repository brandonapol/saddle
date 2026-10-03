package cli

import (
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/spf13/cobra"
)

// stackCollapseCmd is `saddle stack collapse` (#196).
func stackCollapseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "collapse <stack|task|pr>",
		Short: "Merge a stack as one: retarget its top PR to base, wait for CI, squash-merge it",
		Long: `For a stack whose bottom PR can't merge because its fix lives in a PR above
it. Retargets the stack's top PR, which holds every commit below it, to base,
waits for CI on that combined head, and squash-merges it. The tasks it carried
are marked merged, their now-empty PRs closed with a comment, and the rest
restacked. It refuses, changing nothing, when a covered PR is labeled
needs-human or conflicts, or the top PR lacks a lower PR's commits. Red CI on
the combined head puts the top PR back where it was.`,
		Args: cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			res, err := a.CollapseStack(args[0], nil)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "collapsed stack %s: merged %s (%s), carrying %s\n", res.Stack, res.PR, res.Top, strings.Join(res.Covered, ", "))
			for _, pr := range res.Closed {
				fmt.Fprintf(out, "  closed %s\n", pr)
			}
			return nil
		}),
	}
}
