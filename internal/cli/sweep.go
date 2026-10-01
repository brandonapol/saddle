package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/ciwatch"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/sweeper"
	"github.com/spf13/cobra"
)

func sweepCmd() *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "sweep",
		Short: "Merge open saddle PRs that are green, mergeable and tested (opt-in)",
		Long: `Checks every open PR whose head branch starts with saddle/ and merges the
ready ones, bottom of each stack first, retargeting children to the base branch.
Ready means: all checks passed, no conflicts, based on the base branch (or its
parent PR merged), not labeled for review, and tests touched when Go code is.
Risky PRs (go.mod, workflows, very wide changes) get the review label instead.

Off until [sweeper] enabled = true in .saddle/config.toml. --dry-run always
works and only reports.`,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			return runSweep(cmd.Context(), cmd.OutOrStdout(), a.Cfg, dryRun,
				sweeper.Runner(ciwatch.ExecRunner(a.Root)), sweepLogger(a))
		}),
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report what would happen; merge, retarget and label nothing")
	return cmd
}

// runSweep is the command without the app, so tests can inject gh.
func runSweep(ctx context.Context, out io.Writer, cfg config.Config, dryRun bool, gh sweeper.Runner, log func(branch, kind, data string)) error {
	sc := cfg.Sweeper
	dryRun = dryRun || sc.DryRun
	if !sc.Enabled && !dryRun {
		fmt.Fprintln(out, "sweeper is disabled; set [sweeper] enabled = true in .saddle/config.toml, or use --dry-run")
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	o := sweeper.Options{
		GH:          gh,
		Trunk:       cfg.Base,
		Method:      sc.Method,
		ReviewLabel: sc.ReviewLabel,
		DryRun:      dryRun,
	}
	if !dryRun {
		o.Log = log
	}
	rs, err := sweeper.Sweep(ctx, o)
	if err != nil {
		return err
	}
	fmt.Fprint(out, sweeper.Report(rs, dryRun))
	return nil
}

// sweepLogger records sweep events in the store's activity log, under the
// task whose branch the PR heads when saddle knows it.
func sweepLogger(a *app.App) func(branch, kind, data string) {
	tasks := map[string]string{}
	if ts, err := a.Store.Tasks(); err == nil {
		for _, t := range ts {
			tasks[t.Branch] = t.ID
		}
	}
	return func(branch, kind, data string) {
		task := tasks[branch]
		if task == "" {
			data = strings.TrimSpace(branch + " " + data)
		}
		a.Store.Event(task, kind, data)
	}
}
