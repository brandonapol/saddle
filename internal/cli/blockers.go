package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/brandonapol/saddle/internal/app"
)

// blockersGH is GitHub for saddle blockers; tests replace it.
var blockersGH = func(a *app.App) app.BlockerGitHub { return a.BlockerGitHub() }

func blockersCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "blockers",
		Short: "List the open saddle-blocker issues, most frequent first",
		Long: `Saddle files a GitHub issue labelled saddle-blocker for every blocker it
escalates, and comments on it (bumping its count) when the same signature is
seen again (#407). This lists the open ones by how often each was seen: the
repo's "what keeps hurting" view. Turn filing off with [issues] auto_file = false.
See docs/blocker-issues.md.`,
		Args: cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			is, err := app.OpenBlockers(blockersGH(a))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(is) == 0 {
				fmt.Fprintln(out, "No open blocker issues.")
				return nil
			}
			for _, i := range is {
				sig := i.Signature()
				if sig == "" {
					sig = "-"
				}
				fmt.Fprintf(out, "%4d×  #%-5d %-40s %s\n", i.Count(), i.Number, sig, i.Title)
			}
			return nil
		}),
	}
	cmd.AddCommand(blockersHandFixCmd())
	return cmd
}

func blockersHandFixCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "handfix <task> <what you did>",
		Short: "Record the exact hand fix applied to a task's blocker",
		Long: `Records a hand fix against a task. If the task's blocker issue is filed
later, the fix goes in its body; if it was filed already, the fix is added
as a comment on the next sentinel cycle.`,
		Args: cobra.MinimumNArgs(2),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			if _, err := a.Store.Task(args[0]); err != nil {
				return fmt.Errorf("no task %s: %w", args[0], err)
			}
			a.Store.Event(args[0], app.EventHandFix, strings.Join(args[1:], " "))
			fmt.Fprintf(cmd.OutOrStdout(), "Recorded the hand fix for %s.\n", args[0])
			return nil
		}),
	}
}
