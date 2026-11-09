package cli

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/prgraph"
)

// reconcileCmd is the PR-graph engine's shadow (#830, ADR 0009): --plan
// prints what prgraph would do now, --replay replays a state.db offline.
// Neither acts.
func reconcileCmd() *cobra.Command {
	var (
		plan, asJSON           bool
		replay, day, from, til string
		lookback               time.Duration
		hostChecks             []string
	)
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Show what the PR-graph engine would do (--plan), or replay a state.db against it (--replay)",
		Long: `The PR-graph engine (internal/prgraph, ADR 0009) runs in shadow: it plans
but never acts. --plan (the default) prints its plan for the stack as it is
now, reading the forge once. --replay <state.db> replays a database's events
offline, from a copy, and reports where prgraph and saddle's own code
diverged: legacy_only is something saddle did that prgraph wouldn't,
prgraph_only something prgraph would have done that saddle didn't.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if replay == "" {
				if day != "" || from != "" || til != "" {
					return errors.New("--day, --from and --until go with --replay")
				}
				return withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
					in, p, err := a.ShadowPlan(cmd.Context())
					if err != nil {
						return err
					}
					if asJSON {
						return writeJSON(out, p)
					}
					writePlan(out, in, p)
					return nil
				})(cmd, nil)
			}
			if plan {
				return errors.New("--plan and --replay are separate runs")
			}
			f, u, err := replayWindow(day, from, til, time.Local)
			if err != nil {
				return err
			}
			rep, err := app.ReplayShadow(cmd.Context(), replay, app.ReplayOptions{From: f, Until: u, Lookback: lookback,
				Policy: prgraph.Policy{HostChecks: hostChecks}})
			if err != nil {
				return err
			}
			if asJSON {
				return writeJSON(out, rep)
			}
			writeReplay(out, replay, rep)
			return nil
		},
	}
	cmd.Flags().BoolVar(&plan, "plan", false, "print prgraph's plan for the stack now (the default)")
	cmd.Flags().StringVar(&replay, "replay", "", "replay this state.db offline and report divergences")
	cmd.Flags().StringVar(&day, "day", "", "with --replay: the local day to judge, YYYY-MM-DD")
	cmd.Flags().StringVar(&from, "from", "", "with --replay: judge from this time (RFC 3339)")
	cmd.Flags().StringVar(&til, "until", "", "with --replay: judge until this time (RFC 3339)")
	cmd.Flags().DurationVar(&lookback, "lookback", 48*time.Hour, "with --replay: history before the window that builds the starting state")
	cmd.Flags().StringSliceVar(&hostChecks, "host-check", nil, "with --replay: a check the host posts itself, never CI (repeatable)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// replayWindow is the window a replay judges: one local day, or from/until.
func replayWindow(day, from, until string, loc *time.Location) (time.Time, time.Time, error) {
	if day != "" {
		if from != "" || until != "" {
			return time.Time{}, time.Time{}, errors.New("--day or --from/--until, not both")
		}
		d, err := time.ParseInLocation("2006-01-02", day, loc)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("--day: %w", err)
		}
		return d, d.AddDate(0, 0, 1), nil
	}
	if from == "" || until == "" {
		return time.Time{}, time.Time{}, errors.New("a replay needs --day, or --from and --until")
	}
	f, err := time.Parse(time.RFC3339, from)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("--from: %w", err)
	}
	u, err := time.Parse(time.RFC3339, until)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("--until: %w", err)
	}
	if !f.Before(u) {
		return time.Time{}, time.Time{}, errors.New("--from must come before --until")
	}
	return f, u, nil
}

// writePlan prints the settled plan: the actions, then the waits by reason.
func writePlan(w io.Writer, in prgraph.Input, p prgraph.Plan) {
	fmt.Fprintf(w, "prgraph's plan (shadow: nothing is applied) for %d changes:\n", len(in.State.Changes))
	waits := map[string]int{}
	n := 0
	for _, a := range p.Actions {
		switch a.Kind {
		case prgraph.ActWait:
			waits[a.Reason]++
		case prgraph.ActTransition:
		default:
			n++
			fmt.Fprintf(w, "  %s\n", a)
		}
	}
	if n == 0 {
		fmt.Fprintln(w, "  nothing to do")
	}
	if len(waits) > 0 {
		var parts []string
		for _, r := range sortedCounts(waits) {
			parts = append(parts, fmt.Sprintf("%d %s", waits[r], r))
		}
		fmt.Fprintf(w, "waiting: %s\n", strings.Join(parts, ", "))
	}
}

func sortedCounts(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}

func writeReplay(w io.Writer, path string, r app.ReplayReport) {
	stamp := func(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }
	fmt.Fprintf(w, "replay of %s, %s to %s: %d events, %d reconciles, %d divergences\n",
		path, stamp(r.From), stamp(r.Until), r.Events, r.Reconciles, len(r.Divergences))
	o := r.Outcomes
	fmt.Fprintf(w, "outcomes: %d changes, %d merged; legacy repairs %d (%d red only on host checks); prgraph would ask %d fixes; at-risk spells %d\n",
		o.Changes, o.Merged, o.LegacyRepairs, o.LegacyHostRepairs, o.PrgraphFixes, o.AtRiskSpells)
	if r.RecordedTicks > 0 {
		fmt.Fprintf(w, "recorded prgraph ticks: %d, %d replay to another plan\n", r.RecordedTicks, r.RecordedDiffer)
	}
	if r.Unknown > 0 {
		fmt.Fprintf(w, "%d legacy actions were on changes done before the lookback; skipped\n", r.Unknown)
	}
	counts := map[string]int{}
	for _, d := range r.Divergences {
		counts[d.Side+" "+string(d.Kind)]++
	}
	if len(counts) > 0 {
		var parts []string
		for _, k := range sortedCounts(counts) {
			parts = append(parts, fmt.Sprintf("%s %d", k, counts[k]))
		}
		fmt.Fprintf(w, "by kind: %s\n", strings.Join(parts, ", "))
	}
	fmt.Fprintln(w, "divergences:")
	if len(r.Divergences) == 0 {
		fmt.Fprintln(w, "  none")
	}
	for _, d := range r.Divergences {
		fmt.Fprintf(w, "  %s %-6s %s\n", d.At.Local().Format("15:04"), d.Change, d.Line())
	}
	fmt.Fprintf(w, "the replay assumes: %s\n", strings.Join(r.Assumptions, "; "))
}
