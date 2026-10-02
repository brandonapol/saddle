package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/spf13/cobra"
)

// PerfReport is what `saddle perf` measures: how long the polled reads take,
// and how much state has piled up for gc to clear.
type PerfReport struct {
	Reads     []PerfRead     `json:"reads"`
	Tasks     map[string]int `json:"tasks"` // by status
	Rows      map[string]int `json:"rows"`  // by table
	Leftovers map[string]int `json:"leftovers"`
	Kept      map[string]int `json:"kept"` // leftovers gc won't remove
}

// PerfRead is the mean cost of one read over the measured runs.
type PerfRead struct {
	Name     string        `json:"name"`
	Every    string        `json:"every"`
	Mean     time.Duration `json:"mean_ns"`
	GitCalls int64         `json:"git_calls"`
}

func perfCmd() *cobra.Command {
	var runs int
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "perf",
		Short: "Time the reads saddle polls and count the state that has piled up",
		Long: `Times the task list the TUI polls every second and the full status the
orchestrator reads, with the git processes each starts. Then counts tasks by
status, rows per table, and leftovers gc would remove or keep.`,
		Args: cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			r, err := measure(a, max(runs, 1))
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(r)
			}
			w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "READ\tEVERY\tMEAN\tGIT")
			for _, rd := range r.Reads {
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", rd.Name, rd.Every, rd.Mean.Round(10*time.Microsecond), rd.GitCalls)
			}
			fmt.Fprintln(w)
			fmt.Fprintf(w, "tasks\t%s\n", counts(r.Tasks, nil))
			fmt.Fprintf(w, "rows\t%s\n", counts(r.Rows, store.Tables))
			fmt.Fprintf(w, "leftovers\t%s\n", counts(r.Leftovers, nil))
			fmt.Fprintf(w, "kept by gc\t%s\n", counts(r.Kept, nil))
			return w.Flush()
		}),
	}
	cmd.Flags().IntVar(&runs, "runs", 5, "how many times to run each read")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

func measure(a *app.App, runs int) (PerfReport, error) {
	r := PerfReport{Tasks: map[string]int{}, Leftovers: map[string]int{}, Kept: map[string]int{}}
	reads := []struct {
		name, every string
		fn          func() error
	}{
		{"tui tasks", "1s", func() error { _, err := mcpserver.Tasks(a); return err }},
		{"status", "per call", func() error { _, err := mcpserver.Status(a); return err }},
	}
	for _, rd := range reads {
		calls, start := gitx.Calls(), time.Now()
		for range runs {
			if err := rd.fn(); err != nil {
				return r, fmt.Errorf("%s: %w", rd.name, err)
			}
		}
		r.Reads = append(r.Reads, PerfRead{Name: rd.name, Every: rd.every,
			Mean: time.Since(start) / time.Duration(runs), GitCalls: (gitx.Calls() - calls) / int64(runs)})
	}
	ts, err := a.Store.Tasks()
	if err != nil {
		return r, err
	}
	for _, t := range ts {
		r.Tasks[t.Status]++
	}
	if r.Rows, err = a.Store.RowCounts(); err != nil {
		return r, err
	}
	ls, err := a.Leftovers()
	if err != nil {
		return r, err
	}
	for _, l := range ls {
		if l.Keep != "" {
			r.Kept[l.Kind]++
		} else {
			r.Leftovers[l.Kind]++
		}
	}
	return r, nil
}

// counts formats a tally as "a 3, b 1": in the given key order, or largest
// first when keys is nil.
func counts(m map[string]int, keys []string) string {
	if keys == nil {
		for k := range m {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if m[keys[i]] != m[keys[j]] {
				return m[keys[i]] > m[keys[j]]
			}
			return keys[i] < keys[j]
		})
	}
	if len(keys) == 0 {
		return "none"
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}
