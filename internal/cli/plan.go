package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/planner"
	"github.com/spf13/cobra"
)

// planModel picks the planner model: the Messages API when ANTHROPIC_API_KEY
// is set, else the claude CLI on the user's subscription. Tests replace it.
var planModel = func(a *app.App, model string) planner.Model {
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		return &planner.Anthropic{APIKey: key, Model: model}
	}
	if model == "" {
		model = a.Cfg.Claude.Model
	}
	return &planner.ClaudeCLI{Cmd: a.Cfg.Claude.Cmd, Model: model}
}

func planCmd() *cobra.Command {
	var out, model string
	var depth int
	var force bool
	cmd := &cobra.Command{
		Use:   "plan <epic.md|-|gh:#N|gh:owner/repo#N>",
		Short: "Plan an epic into tasks, then review, edit and approve the plan",
		Long: `Plan an epic into tasks with the planner model and save the plan as TOML
under .saddle/plans/. Review it with show, change it with edit (opens
$EDITOR, then re-runs the checker) or replan --note, and freeze it at the
base commit with approve.`,
		Args: cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			src, err := planner.ParseSource(args[0])
			if err != nil {
				return err
			}
			epic, err := planner.LoadEpic(cmd.Context(), src, cmd.InOrStdin(), issueFetcher(a))
			if err != nil {
				return err
			}
			path := out
			if path == "" {
				path = filepath.Join(a.Root, ".saddle", "plans", slug(epic.Title)+".toml")
			}
			if _, err := os.Stat(path); err == nil && !force {
				return fmt.Errorf("%s exists; edit or replan it, or pass --force to start over", path)
			}
			doc := planner.Doc{Epic: epic.Title, Source: args[0], Repo: epic.Repo, Issue: epic.Issue, Text: epic.Text}
			return generate(cmd, a, path, doc, nil, "", model, depth)
		}),
	}
	cmd.Flags().StringVarP(&out, "out", "o", "", "plan file (default .saddle/plans/<epic>.toml)")
	cmd.Flags().StringVar(&model, "model", "", "planner model (default "+planner.DefaultModel+" with ANTHROPIC_API_KEY, else claude.model)")
	cmd.Flags().IntVar(&depth, "depth", 3, "repo tree depth shown to the planner")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing plan file")
	cmd.AddCommand(planShowCmd(), planEditCmd(), planReplanCmd(), planApproveCmd(), planReopenCmd())
	return cmd
}

func generate(cmd *cobra.Command, a *app.App, path string, doc planner.Doc, prev []planner.Task, note, model string, depth int) error {
	snap, err := planner.TakeSnapshot(cmd.Context(), a.Root, depth)
	if err != nil {
		return err
	}
	snap.Serial = a.Cfg.Serial
	req := planner.Request{Epic: doc.Text, Snapshot: snap, Previous: prev, Note: note}
	fmt.Fprintln(cmd.ErrOrStderr(), "planning…")
	d, err := planner.Generate(cmd.Context(), planModel(a, model), req, a.Cfg.Concurrency)
	if err != nil {
		return err
	}
	doc.Tasks = d.Tasks
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := planner.WriteDoc(path, doc, a.Cfg.Serial, a.Cfg.Concurrency); err != nil {
		return err
	}
	return showPlan(cmd.OutOrStdout(), a, path)
}

func showPlan(w io.Writer, a *app.App, path string) error {
	d, err := planner.LoadDoc(path)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "%s\n\n%s", path, planner.Render(d, a.Cfg.Serial, a.Cfg.Concurrency))
	return nil
}

func planShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <plan.toml>",
		Short: "Print a plan with the checker's waves, train routes and barriers",
		Args:  cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			return showPlan(cmd.OutOrStdout(), a, args[0])
		}),
	}
}

func planEditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "edit <plan.toml>",
		Short: "Edit a plan in $EDITOR, then re-run the checker",
		Args:  cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			_, _, err := planner.Edit(args[0], shellEditor(cmd), a.Cfg.Serial, a.Cfg.Concurrency)
			if errors.Is(err, planner.ErrApproved) {
				return fmt.Errorf("%w: saddle plan reopen %s", err, args[0])
			}
			if err != nil {
				return fmt.Errorf("%w\nyour edit is kept; run saddle plan edit %s again to fix it", err, args[0])
			}
			return showPlan(cmd.OutOrStdout(), a, args[0])
		}),
	}
}

func planReplanCmd() *cobra.Command {
	var note, model string
	var depth int
	cmd := &cobra.Command{
		Use:   "replan <plan.toml> --note <what to change>",
		Short: "Ask the planner to revise a plan as the note says",
		Args:  cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			if strings.TrimSpace(note) == "" {
				return errors.New("replan needs --note saying what to change")
			}
			d, err := planner.LoadDoc(args[0])
			if err != nil {
				return err
			}
			if d.Approved {
				return fmt.Errorf("%w: saddle plan reopen %s", planner.ErrApproved, args[0])
			}
			return generate(cmd, a, args[0], d, d.Tasks, note, model, depth)
		}),
	}
	cmd.Flags().StringVar(&note, "note", "", "what the planner should change")
	cmd.Flags().StringVar(&model, "model", "", "planner model")
	cmd.Flags().IntVar(&depth, "depth", 3, "repo tree depth shown to the planner")
	return cmd
}

func planApproveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "approve <plan.toml>",
		Short: "Check a plan and freeze it at the current base commit",
		Args:  cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			base, err := gitx.RevParse(a.Root, a.Cfg.Base)
			if err != nil {
				return err
			}
			d, err := planner.Approve(args[0], base, a.Cfg.Serial, a.Cfg.Concurrency)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "approved %s: %d tasks frozen at %s %s\n", args[0], len(d.Tasks), a.Cfg.Base, base)
			return nil
		}),
	}
}

func planReopenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reopen <plan.toml>",
		Short: "Unfreeze an approved plan so it can be edited again",
		Args:  cobra.ExactArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			if _, err := planner.Reopen(args[0], a.Cfg.Serial, a.Cfg.Concurrency); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "reopened %s\n", args[0])
			return nil
		}),
	}
}

// shellEditor runs $VISUAL or $EDITOR (vi if neither is set) on the plan,
// through sh so editor settings with flags work.
func shellEditor(cmd *cobra.Command) planner.Editor {
	return func(path string) error {
		ed := os.Getenv("VISUAL")
		if ed == "" {
			ed = os.Getenv("EDITOR")
		}
		if ed == "" {
			ed = "vi"
		}
		c := exec.Command("sh", "-c", ed+` "$1"`, "sh", path)
		c.Stdin, c.Stdout, c.Stderr = os.Stdin, cmd.OutOrStdout(), cmd.ErrOrStderr()
		return c.Run()
	}
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(title string) string {
	s := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(title), "-"), "-")
	if len(s) > 50 {
		s = strings.TrimRight(s[:50], "-")
	}
	if s == "" {
		return "plan"
	}
	return s
}

// issueFetcher lets planner epic sources read GitHub issues through gh.
func issueFetcher(a *app.App) planner.IssueFetcher {
	return func(_ context.Context, repo string, n int) (planner.Issue, error) {
		t, err := a.TicketIn(repo, n)
		if err != nil {
			return planner.Issue{}, err
		}
		return ticketIssue(t), nil
	}
}

func ticketIssue(t app.Ticket) planner.Issue {
	is := planner.Issue{Number: t.Number, Title: t.Title, State: t.State, Body: t.Body, URL: t.URL}
	for _, s := range t.SubIssues {
		is.Subs = append(is.Subs, planner.Issue{Number: s.Number, Title: s.Title, State: s.State, Body: s.Body})
	}
	return is
}
