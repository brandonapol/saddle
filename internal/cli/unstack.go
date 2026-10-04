package cli

import (
	"fmt"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/sentinel"
	"github.com/spf13/cobra"
)

// The stack's escape hatches (#119): each replaces a hand edit of state.db.

func unstackCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "unstack <task|pr>...",
		Short: "Take landed tasks out of the PR stack for good",
		Long: `Takes landed tasks out of the PR stack, by task id, PR URL or PR number (#12).
Their train entries become superseded, the next restack drops their commits
from the integration branch, and Saddle never touches their PRs again. Merged
and closed PRs and killed tasks leave the stack by themselves; this is for the
rest. The stack sentinel checks again right away.`,
		Args: cobra.MinimumNArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			for _, ref := range args {
				t, err := a.Unstack(ref)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s is out of the PR stack\n", t.ID)
			}
			reportCheck(cmd, a)
			fmt.Fprintln(cmd.OutOrStdout(), "run restack to drop their commits from "+a.Cfg.Integration)
			return nil
		}),
	}
}

func sentinelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sentinel",
		Short: "Check the PR stack, or acknowledge its at-risk flag",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "check",
		Short: "Run one stack sentinel check now",
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			reportCheck(cmd, a)
			return nil
		}),
	}, &cobra.Command{
		Use:   "ack",
		Short: "Acknowledge the stack's at-risk flag and ci-red holds so they stop holding work back",
		Long: `Acknowledges the stack sentinel's current flag when restack can't fix it: prs
and land stop holding work back and the needs-human labels come off. The flag
comes back if a different layer breaks; it goes once the stack checks clean.
It also acknowledges every ci-red layer (#213): prs and land stop holding the
work above it until that layer goes red on a new head. Auto-merge still never
merges a red PR or one above it.`,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			red, err := a.AckCIRed()
			if err != nil {
				return err
			}
			for _, l := range red {
				fmt.Fprintf(cmd.OutOrStdout(), "acknowledged ci-red on %s: prs and land stop holding the layers above it\n", l.Task)
			}
			f, err := a.AckFlag()
			if err != nil {
				if len(red) > 0 {
					return nil // only red CI was holding work back
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "acknowledged the flag from %s up: %s\n", f.Task, f.Cause)
			reportCheck(cmd, a)
			return nil
		}),
	})
	return cmd
}

func requeueCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "requeue <task>",
		Short: "Put a landed task whose work integration lacks back in the merge train",
		Args:  cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			if err := a.Requeue(args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is queued again; run land\n", args[0])
			return nil
		}),
	}
}

// reportCheck runs one sentinel check and prints what it found.
func reportCheck(cmd *cobra.Command, a *app.App) {
	rep, err := sentinel.New(a).Check()
	out := cmd.OutOrStdout()
	switch {
	case err != nil:
		fmt.Fprintln(cmd.ErrOrStderr(), "sentinel: "+err.Error())
	case rep.Busy:
		fmt.Fprintln(out, "the train is busy; the sentinel checks again shortly")
	case rep.Acked:
		fmt.Fprintf(out, "stack still at risk from %s up (acknowledged): %s\n", rep.Task, rep.Cause)
	case rep.AtRisk:
		fmt.Fprintf(out, "stack at risk from %s up: %s\n", rep.Task, rep.Cause)
	default:
		fmt.Fprintln(out, "the stack checks clean")
	}
}
