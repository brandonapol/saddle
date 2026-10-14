package cli

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/spf13/cobra"
)

func decisionsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "decisions",
		Short: "List the decisions that wait on you, and what saddle decided without you",
		Long: `The decision queue (#464). Owner-gated actions (a new dependency, a release
tag, a gate tolerance) are filed as decision cards with options, a recommended
default and whether that default can be undone, and unrelated work keeps
running meanwhile. A reversible card runs its recommended option once its
budget ([stall.kinds] owner, default 30m) runs out; an irreversible one waits
for you. A [[preauth]] rule allows a class of action up front:

  [[preauth]]
  class = "dependency"
  match = "github.com/aws/*"   # a glob over the subject; empty allows all
  note = "SDK packages listed in ADR 7"

Once a day, at [decisions] digest_at (default 08:00), saddle sends one digest
of what waits on you and what it decided meanwhile.`,
		Args: cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			cs, err := a.Decisions()
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(cs) == 0 {
				fmt.Fprintln(out, "No decisions.")
				return nil
			}
			for _, c := range cs {
				if c.Open() {
					fmt.Fprintf(out, "[%s]\n%s\n\n", c.ID, c.Render())
				}
			}
			for _, c := range cs {
				if !c.Open() {
					fmt.Fprintln(out, c.Line())
				}
			}
			return nil
		}),
	}
	cmd.AddCommand(decisionsFileCmd(), decisionsAnswerCmd(), decisionsAllowCmd(), decisionsDigestCmd())
	return cmd
}

// parseOptions reads --option values, "label: action" each.
func parseOptions(vals []string) ([]app.DecisionOption, error) {
	var opts []app.DecisionOption
	for _, v := range vals {
		label, action, ok := strings.Cut(v, ": ")
		if !ok || strings.TrimSpace(label) == "" || strings.TrimSpace(action) == "" {
			return nil, fmt.Errorf("--option %q: want \"label: action\"", v)
		}
		opts = append(opts, app.DecisionOption{Label: strings.TrimSpace(label), Action: strings.TrimSpace(action)})
	}
	return opts, nil
}

func decisionsFileCmd() *cobra.Command {
	var req app.DecisionReq
	var options []string
	cmd := &cobra.Command{
		Use:   "file <question>",
		Short: "Queue a decision for the owner",
		Long: `Queues an owner decision. Give each option as --option "label: action" and
pick the default with --recommend (1-based, default 1). --irreversible marks
an action that can't be undone (a tag push, a publish): saddle never runs it
without the owner or a [[preauth]] rule. Filing the same decision again while
it is open changes nothing.`,
		Args: cobra.MinimumNArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			opts, err := parseOptions(options)
			if err != nil {
				return err
			}
			req.Options, req.Question = opts, strings.Join(args, " ")
			req.By = app.OrchestratorID
			if t := os.Getenv("SADDLE_TASK"); t != "" {
				req.By = t
			}
			c, err := a.FileDecision(req, time.Now())
			if err != nil {
				return err
			}
			if c.Open() {
				fmt.Fprintf(cmd.OutOrStdout(), "[%s]\n%s\n", c.ID, c.Render())
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), c.Line())
			return nil
		}),
	}
	f := cmd.Flags()
	f.StringVar(&req.Task, "task", "", "the task it blocks (default: yours, or the orchestrator)")
	f.StringVar(&req.Class, "class", "", "the kind of action, e.g. dependency, release-tag, gate-tolerance")
	f.StringVar(&req.Subject, "subject", "", "what it acts on, e.g. a module path or a tag")
	f.StringVar(&req.What, "what", "", "one line of context")
	f.StringVar(&req.Blocking, "blocking", "", "one line on what it holds up")
	f.StringArrayVar(&options, "option", nil, `an option, "label: action"; repeat for each`)
	f.IntVar(&req.Recommended, "recommend", 0, "the recommended option, 1-based (default 1)")
	f.BoolVar(&req.Irreversible, "irreversible", false, "the recommended option can't be undone, so it never runs on a timeout")
	f.DurationVar(&req.Timeout, "timeout", 0, "how long a reversible card waits before its default runs (default [stall.kinds] owner)")
	return cmd
}

func decisionsAnswerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "answer <id> <option>",
		Short: "Answer a decision by its id",
		Args:  cobra.ExactArgs(2),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			n, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("option %q: want a number", args[1])
			}
			c, err := a.AnswerDecisionID(args[0], n, time.Now())
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), c.Line())
			return nil
		}),
	}
}

func decisionsAllowCmd() *cobra.Command {
	var r app.PreauthRule
	cmd := &cobra.Command{
		Use:   "allow <class> [match]",
		Short: "Pre-authorize a class of decision for good",
		Long: `Adds a [[preauth]] rule to .saddle/config.toml: decisions of class whose
subject matches the glob (all of them without one) take their recommended
option, or --option, without you. The decisions waiting now that it allows
close at once and their tasks go ahead.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			r.Class = args[0]
			if len(args) > 1 {
				r.Match = args[1]
			}
			done, err := a.AllowClass(r, time.Now())
			if err != nil && len(done) == 0 {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Added %s.\n", r)
			for _, c := range done {
				fmt.Fprintln(out, c.Line())
			}
			return err
		}),
	}
	cmd.Flags().IntVar(&r.Option, "option", 0, "the option it takes, 1-based (default: the recommended one)")
	cmd.Flags().StringVar(&r.Note, "note", "", "why you allow it, e.g. \"listed in ADR 7\"")
	return cmd
}

func decisionsDigestCmd() *cobra.Command {
	var since time.Duration
	cmd := &cobra.Command{
		Use:   "digest",
		Short: "Print the decisions digest now",
		Args:  cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			now := time.Now()
			text, err := a.DecisionDigest(now.Add(-since), now)
			if err != nil {
				return err
			}
			if text == "" {
				text = "Nothing waits on you and saddle decided nothing without you."
			}
			fmt.Fprintln(cmd.OutOrStdout(), text)
			return nil
		}),
	}
	cmd.Flags().DurationVar(&since, "since", 24*time.Hour, "how far back to list what saddle decided without you")
	return cmd
}
