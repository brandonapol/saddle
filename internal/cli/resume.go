package cli

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/spf13/cobra"
)

func resumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <task>",
		Short: "Give an orphaned or paused task a new window, resuming its agent session",
		Long: `Opens a new tmux window in the task's worktree. A Claude Code task with a
stored session continues it (claude --resume); any other is relaunched with
its original brief and a note that work may already exist in the worktree.
saddle up does this by itself for every task whose window is gone.`,
		Args: cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			r, err := a.Resume(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "resumed %s in window %s\n", r, r.Window)
			return nil
		}),
	}
}

func rescueCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rescue <task>",
		Short: "Save a task's uncommitted work to rescue/<task> and kill it",
		Long: `Commits the task's uncommitted changes (untracked files too) on top of its
branch as rescue/<task>, writes them as a patch under .saddle/rescue/, then
kills the task: its claims are released and its worktree is kept. Restore
the files with git apply .saddle/rescue/<task>.diff or from the branch.`,
		Args: cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			r, err := a.Rescue(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s killed; its work is on %s\n", r.Task, r.Branch)
			if r.Diff != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "uncommitted changes: %s\n", r.Diff)
			}
			return nil
		}),
	}
}

// pauseOnTerm pauses every live task when saddle up got SIGTERM, so a
// restart resumes them instead of leaving them running with no window. It
// returns nil when no SIGTERM came.
func pauseOnTerm(term <-chan os.Signal, a *app.App) ([]string, error) {
	select {
	case <-term:
	default:
		return nil, nil
	}
	paused, err := a.Pause()
	if paused == nil {
		paused = []string{}
	}
	return paused, err
}

// writePaused says which tasks saddle down or SIGTERM paused.
func writePaused(out io.Writer, paused []string) {
	if len(paused) == 0 {
		fmt.Fprintln(out, "no running agents to pause")
		return
	}
	fmt.Fprintf(out, "paused %d agents: %s (worktrees and claims kept; saddle up resumes them)\n", len(paused), strings.Join(paused, ", "))
}
