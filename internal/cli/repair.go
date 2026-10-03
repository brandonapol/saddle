package cli

import (
	"fmt"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/spf13/cobra"
)

func repairCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "repair <task|pr>",
		Short: "Spawn a repair task to re-land a task's conflicting work on a fresh branch",
		Long: `Spawns a repair task for a task whose landed commits (or queued branch)
conflict and whose agent can't resolve it (#172). Restack and land do this by
themselves when the owner's window is gone; this triggers it by hand, for
example when the owner is alive but stuck. A stacked task leaves the stack as
repairing and restack runs, so the repair is cut from integration without its
commits. The repair is seeded with the task's branch, commits, conflicting
files, base and prompt, claims those files and the task's claims, and lands
normally; then the task is superseded and its PR closed. A task gets at most
one live repair: running this again reports the one it has.`,
		Args: cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			res, err := a.Repair(args[0], force)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			switch {
			case res.Repair == "":
				fmt.Fprintf(out, "%s is out of the stack for repair; %s\n", res.Task, res.Note)
			case res.Created:
				fmt.Fprintf(out, "spawned %s to repair %s\n", res.Repair, res.Task)
			default:
				fmt.Fprintf(out, "%s is already repairing %s\n", res.Repair, res.Task)
			}
			return nil
		}),
	}
	cmd.Flags().BoolVar(&force, "force", false, "spawn past the concurrency limit and claim overlaps")
	return cmd
}
