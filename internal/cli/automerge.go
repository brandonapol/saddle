package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/spf13/cobra"
)

// automergeCmd steers the auto-merge watcher (#152).
func automergeCmd() *cobra.Command {
	var asJSON bool
	status := func(cmd *cobra.Command, a *app.App, _ []string) error {
		return printAutomerge(cmd.OutOrStdout(), a, asJSON)
	}
	cmd := &cobra.Command{
		Use:   "automerge",
		Short: "Turn auto-merge of ready PR stacks on or off, hold or release a stack",
		Long: `While saddle up runs, the auto-merge watcher can merge the bottom PR of a ready
stack (checks green, mergeable and CLEAN, not a draft, not needs-human, not
flagged at risk), restack, and repeat until the stack is empty. It is off
unless [train] auto_merge or 'saddle automerge on' turns it on; the runtime
switch survives restarts. A held stack is never merged but stays tracked and
restacked. Any failed merge stops the watcher; 'on' resumes it.`,
		RunE: withApp(status),
	}
	cmd.PersistentFlags().BoolVar(&asJSON, "json", false, "print JSON")
	toggle := func(on bool) func(*cobra.Command, *app.App, []string) error {
		return func(cmd *cobra.Command, a *app.App, _ []string) error {
			if err := a.NewAutomerge(nil).SetEnabled(on); err != nil {
				return err
			}
			if on {
				fmt.Fprintln(cmd.OutOrStdout(), "auto-merge is on: ready stacks merge bottom-up while saddle up runs")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "auto-merge is off: saddle leaves PRs for you to merge")
			}
			return nil
		}
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "on",
		Short: "Merge ready stacks automatically (also resumes after a failure)",
		Args:  cobra.NoArgs,
		RunE:  withApp(toggle(true)),
	}, &cobra.Command{
		Use:   "off",
		Short: "Stop merging; leave PRs for the owner",
		Args:  cobra.NoArgs,
		RunE:  withApp(toggle(false)),
	}, &cobra.Command{
		Use:   "status",
		Short: "Show whether auto-merge is on, the stacks, and what it waits on",
		Args:  cobra.NoArgs,
		RunE:  withApp(status),
	}, &cobra.Command{
		Use:   "hold <stack|task|pr>",
		Short: "Never auto-merge this stack until it is released; keep tracking it",
		Args:  cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			t, err := a.AutomergeHold(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s's stack is held; `saddle automerge release %s` lets it merge\n", t.ID, t.ID)
			return nil
		}),
	}, &cobra.Command{
		Use:   "release <stack|task|pr>",
		Short: "Let a held stack auto-merge again",
		Args:  cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			t, err := a.AutomergeRelease(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s's stack is released\n", t.ID)
			return nil
		}),
	})
	return cmd
}

// stackCmd shows the PR stacks as a graph, rebases one, and manages the
// owner's custom stacks (see customStackCmds).
func stackCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "stack",
		Short: "Show the PR stacks as a graph: bases, CI, mergeability, holds, how far behind base",
		Args:  cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			return printAutomerge(cmd.OutOrStdout(), a, asJSON)
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.AddCommand(&cobra.Command{
		Use:   "rebase <stack|task|pr>",
		Short: "Rebase a stack onto origin's base, held or not; a conflict goes back to its task",
		Long: `Rebases the stack holding the given task or PR onto origin's base and reports
what moved. The integration branch is one linear history, so this runs
restack: every stacked task replays in train order and nothing moves unless
all apply. A conflict moves nothing and goes back to the task that owns the
commit; saddle never resolves it.`,
		Args: cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			res, err := a.RebaseStack(args[0])
			var c *app.RestackConflict
			if errors.As(err, &c) {
				return fmt.Errorf("%w\n%s has the conflict; rebase again once it is resolved", err, c.Task)
			}
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "rebased stack %s (%s) onto origin at %s\n", res.Stack, strings.Join(res.Tasks, ", "), short(res.Restack.Base))
			if len(res.Moves) == 0 {
				fmt.Fprintln(out, "  nothing in it moved; it was already on base")
			}
			for _, m := range res.Moves {
				fmt.Fprintf(out, "  %s: %s %s → %s\n", m.Task, m.Ref, short(m.Old), short(m.New))
			}
			if n := len(res.Restack.Moves) - len(res.Moves); n > 0 {
				fmt.Fprintf(out, "  %d other refs moved with it (integration is linear)\n", n)
			}
			return nil
		}),
	}, stackCollapseCmd())
	cmd.AddCommand(customStackCmds()...)
	return cmd
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// printAutomerge prints the live graph, or the last check's when GitHub
// can't be read.
func printAutomerge(out io.Writer, a *app.App, asJSON bool) error {
	if !asJSON {
		gate, err := a.Gate()
		if err != nil {
			return err
		}
		if gate.Environment != "" {
			fmt.Fprintln(out, gate.Environment)
		}
		for _, red := range gate.Red {
			fmt.Fprintf(out, "pre-publish gate holds %s: %s (`%s`) in %s\n%s\n", red.Task, red.Check.Name, red.Check.Cmd, red.Directory, red.Tail)
		}
	}
	st, err := a.NewAutomerge(nil).Plan()
	stale := ""
	if err != nil {
		if st, err = a.AutomergeState(); err != nil {
			return err
		}
		stale = " (GitHub unreachable; showing the last check)"
	}
	if asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	writeAutomerge(out, st, a.Cfg.Base)
	if stale != "" {
		fmt.Fprintln(out, strings.TrimSpace(stale))
	}
	return nil
}

func writeAutomerge(out io.Writer, st automerge.Status, base string) {
	state := "off"
	if st.Enabled {
		state = "on"
	}
	fmt.Fprintf(out, "auto-merge: %s (%s)\n", state, st.Source)
	if st.Stopped != "" {
		fmt.Fprintf(out, "stopped: %s\n  run `saddle automerge on` to resume\n", st.Stopped)
	}
	if len(st.Holds) > 0 {
		fmt.Fprintf(out, "held: %s\n", strings.Join(st.Holds, ", "))
	}
	if st.Busy {
		fmt.Fprintf(out, "train lock busy since %s (a land, restack or the stack sentinel holds it)\n", st.BusySince.Local().Format("15:04:05"))
	}
	if !st.Next.IsZero() {
		fmt.Fprintf(out, "next check: %s\n", st.Next.Local().Format("15:04:05"))
	}
	if len(st.Stacks) == 0 {
		fmt.Fprintln(out, "no open PR stacks")
		return
	}
	for _, s := range st.Stacks {
		var tags []string
		if s.Held {
			tags = append(tags, "held")
		}
		if s.Behind > 0 {
			tags = append(tags, fmt.Sprintf("%d behind %s; `saddle stack rebase %s`", s.Behind, base, s.ID))
		}
		line := "stack " + s.ID
		if len(tags) > 0 {
			line += " [" + strings.Join(tags, ", ") + "]"
		}
		fmt.Fprintln(out, line)
		for i, n := range s.Nodes {
			fmt.Fprintf(out, "  %d. %s %s → %s", i+1, n.Task, n.PR, n.Base)
			switch {
			case n.Error != "":
				fmt.Fprintf(out, "  (can't read: %s)", n.Error)
			default:
				fmt.Fprintf(out, "  checks %s, %s/%s", n.Checks, strings.ToLower(n.Mergeable), strings.ToLower(n.MergeState))
				if n.Draft {
					fmt.Fprint(out, ", draft")
				}
				if len(n.Labels) > 0 {
					fmt.Fprintf(out, ", labels %s", strings.Join(n.Labels, ","))
				}
			}
			fmt.Fprintln(out)
		}
		switch {
		case s.Ready && s.Blocked != "":
			fmt.Fprintf(out, "  next: %s is ready to merge; %s\n", s.Next, s.Blocked)
		case s.Ready:
			fmt.Fprintf(out, "  next: %s is ready to merge\n", s.Next)
		default:
			fmt.Fprintf(out, "  next: %s waits: %s\n", s.Next, s.Why)
		}
	}
}
