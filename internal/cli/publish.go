package cli

import (
	"fmt"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/spf13/cobra"
)

func publishCmd() *cobra.Command {
	var req app.PublishReq
	cmd := &cobra.Command{
		Use:   "publish <task|branch>",
		Short: "Push a task's own commits as an independent PR against base, outside the stack",
		Long: `Publishes one task's own commits, and nothing else, as an independent PR
against base (#220). It is the escape hatch for when the PR stack is broken
but this one piece of work is fine: saddle replays the task's commits onto
base, pushes them to a fresh branch itself (the model never runs git push) and
opens the PR with the repo's PR template and "Closes #N".

It refuses when the commits need work from tasks below it that base doesn't
have, when a branch carries another task's commits, or when the branch name is
one saddle already uses. When the task already has a PR it prints its URL.
prs leaves a published task's PR alone; the layers above it wait until it
merges.`,
		Args: cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			req.Target = args[0]
			res, err := a.Publish(req)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			name := res.Task
			if name == "" {
				name = req.Target
			}
			if res.Existing && res.Branch == "" {
				fmt.Fprintf(out, "%s already has a PR: %s\n", name, res.URL)
				return nil
			}
			if res.Existing {
				fmt.Fprintf(out, "pushed %s; its PR was already open: %s\n", res.Branch, res.URL)
				return nil
			}
			commits := fmt.Sprintf("%d commits", res.Commits)
			if res.Commits == 1 {
				commits = "1 commit"
			}
			fmt.Fprintf(out, "published %s (%s) as %s: %s\n", name, commits, res.Branch, res.URL)
			return nil
		}),
	}
	cmd.Flags().StringVar(&req.Base, "base", "", "the branch the PR targets (default: the configured base)")
	cmd.Flags().StringVar(&req.BranchName, "branch-name", "", "the branch to push (default: saddle/<slug of the title>)")
	cmd.Flags().BoolVar(&req.Draft, "draft", false, "open the PR as a draft")
	return cmd
}
