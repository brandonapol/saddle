package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/runq"
	"github.com/spf13/cobra"
)

const statsSinceDefault = 7 * 24 * time.Hour

// parseSince reads a window like 7d, 12h or 90m.
func parseSince(s string) (time.Duration, error) {
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.ParseFloat(n, 64)
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("--since %q: want a positive duration like 7d, 12h or 90m", s)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("--since %q: want a positive duration like 7d, 12h or 90m", s)
	}
	return d, nil
}

// statsDur renders a millisecond count as 3m10s, 42s or 1h05m.
func statsDur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		d = d.Round(time.Second)
		return fmt.Sprintf("%dm%02ds", int(d/time.Minute), int(d%time.Minute/time.Second))
	}
	return app.RoughDuration(d)
}

func statsRSS(kb int64) string {
	switch {
	case kb <= 0:
		return "-"
	case kb >= 1<<20:
		return fmt.Sprintf("%.1fG", float64(kb)/(1<<20))
	}
	return fmt.Sprintf("%dM", kb>>10)
}

func runqStatsCmd() *cobra.Command {
	var since string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "stats [--since 7d] [--json]",
		Short: "Per-class run counts, durations, cores, memory and overlap from history",
		Long: `Reports finished heavy runs per class over the window (default 7d): run
count, p50/p95 time held and time waited, average cores (CPU time over time
held), p95 peak memory, and the share of the class's run time during which
another heavy run was also running. History older than 30 days is pruned.`,
		Args: cobra.NoArgs,
		RunE: withQueue(func(c *cobra.Command, q *runq.Queue, _ []string) error {
			window, err := parseSince(since)
			if err != nil {
				return err
			}
			st, err := q.Stats(window)
			if err != nil {
				return err
			}
			out := c.OutOrStdout()
			if asJSON {
				if st == nil {
					st = []runq.ClassStats{}
				}
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(struct {
					SinceMS int64             `json:"since_ms"`
					Classes []runq.ClassStats `json:"classes"`
				}{window.Milliseconds(), st})
			}
			writeRunqStats(out, st, since)
			return nil
		}),
	}
	cmd.Flags().StringVar(&since, "since", "7d", "window: how far back to look (7d, 12h, ...)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func writeRunqStats(out io.Writer, st []runq.ClassStats, since string) {
	if len(st) == 0 {
		fmt.Fprintf(out, "no heavy runs in the last %s\n", since)
		return
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "CLASS\tRUNS\tHELD p50\tHELD p95\tWAIT p50\tWAIT p95\tCORES\tRSS p95\tOVERLAP")
	for _, s := range st {
		fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%s\t%.1f\t%s\t%d%%\n", s.Class, s.Runs,
			statsDur(s.HeldP50MS), statsDur(s.HeldP95MS), statsDur(s.WaitedP50MS), statsDur(s.WaitedP95MS),
			s.AvgCores, statsRSS(s.RSSP95KB), int(s.Overlap*100+0.5))
	}
	_ = w.Flush()
}

// statsSummary is the saddle status line, or "" with no history:
// "heavy runs (7d): go-test 42 runs p50 3m10s 2.1 cores; ...".
func statsSummary(st []runq.ClassStats) string {
	parts := make([]string, 0, len(st))
	for _, s := range st {
		parts = append(parts, fmt.Sprintf("%s %d runs p50 %s %.1f cores", s.Class, s.Runs, statsDur(s.HeldP50MS), s.AvgCores))
	}
	if len(parts) == 0 {
		return ""
	}
	return "heavy runs (7d): " + strings.Join(parts, "; ")
}

// writeHeavyStatsLine prints the one-line 7d history summary under status.
// It is best effort: no queue or no history prints nothing.
func writeHeavyStatsLine(out io.Writer, a *app.App) {
	q, _, err := a.Heavy().Open()
	if err != nil {
		return
	}
	defer func() { _ = q.Close() }()
	st, err := q.Stats(statsSinceDefault)
	if err != nil {
		return
	}
	if line := statsSummary(st); line != "" {
		fmt.Fprintln(out, "\n"+line)
	}
}
