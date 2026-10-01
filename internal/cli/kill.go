package cli

import (
	"github.com/brandonapol/saddle/internal/app"
	"github.com/spf13/cobra"
)

func killCmd() *cobra.Command {
	var keep bool
	cmd := &cobra.Command{
		Use:   "kill <task>",
		Short: "Stop a task's agent, release its claims and remove its worktree",
		Long: `Stops a task's agent and releases its claims. Its worktree is removed, and so is
its branch when it has no commits beyond the integration branch. A branch with
commits, or a worktree with uncommitted changes, is always kept.`,
		Args: cobra.ExactArgs(1),
		RunE: withApp(func(_ *cobra.Command, a *app.App, args []string) error {
			return a.Kill(args[0], keep)
		}),
	}
	cmd.Flags().BoolVar(&keep, "keep", false, "keep the worktree and branch")
	return cmd
}
