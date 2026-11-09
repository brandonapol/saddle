package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/brandonapol/saddle/internal/app"
)

// flattenCmd is `saddle flatten [--dry-run]` (#937): the one-time move of
// a repo's open PRs from the old merge train onto main (ADR 0012).
func flattenCmd() *cobra.Command {
	var dry, asJSON bool
	c := &cobra.Command{
		Use:   "flatten",
		Short: "Move the old train's open stacked PRs onto main: each with only its task's own commits",
		Long: `flatten moves a repo that ran the old merge train onto one task, one branch,
one PR (ADR 0012). For each open PR of a landed task, bottom of each stack
first, it finds the task's own commits (its Saddle-Task trailer, else the
commits in <PR base>..<head> that aren't on main, recorded by the train for
another task, or in a PR below it), replays them onto main on the task's own
branch, force-pushes it, retargets the PR to main and rewrites its body to
the task's summary. The task becomes pr_open and the merge queue takes it.

A replay that conflicts leaves the PR as it was and reopens the task as
fixing. A PR whose commits are all on main is closed with a comment naming
them. A commit nobody can be sure of leaves its PR alone, with the reason.
A repair's PR keeps the repair's task, drops "Repair tN " from its title,
and its original is killed (killed_by superseded_by_flatten).

After a real run legacy_train is set to false if it was on. --dry-run
prints the plan, per PR, and changes nothing.`,
		Args: cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			res, err := a.Flatten(cmd.Context(), nil, dry)
			if asJSON {
				b, jerr := json.MarshalIndent(res, "", "  ")
				if jerr != nil {
					return jerr
				}
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), res.Text())
			return err
		}),
	}
	c.Flags().BoolVar(&dry, "dry-run", false, "print what would happen, per PR, and change nothing")
	c.Flags().BoolVar(&asJSON, "json", false, "print the plan as JSON")
	return c
}
