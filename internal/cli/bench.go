package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/brandonapol/saddle/internal/bench"
	"github.com/spf13/cobra"
)

// benchCmd runs the collision bench (#599, docs/bench.md).
func benchCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "bench",
		Short: "Run the collision bench: dummy tickets built to make parallel agents collide, scored",
		Long: `The bench works a scenario's dummy tickets with autopilot in a hermetic
world (temp HOME, fake GitHub, private tmux) on a copy of bench/fixture, then
scores the run: denied writes, claim waits, merge-train bounces, restacks,
forecast hits and misses, tickets that needed help, and whether the fixture's
tests pass on the integration branch. Run it from a saddle source checkout:
it builds saddle, the fake gh and the fake agent from it. See docs/bench.md.`,
	}
	cmd.PersistentFlags().StringVar(&dir, "dir", "", "the bench directory (default: bench/ in this checkout)")

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the scenarios",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, err := benchDir(dir)
			if err != nil {
				return err
			}
			names, err := bench.List(d)
			if err != nil {
				return err
			}
			for _, n := range names {
				sc, err := bench.Load(d, n)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-14s %d tickets, overlap %s: %s\n", n, len(sc.Tickets), sc.Overlap, sc.Description)
			}
			return nil
		},
	})

	var o bench.Options
	run := &cobra.Command{
		Use:   "run <scenario>",
		Short: "Run one scenario end to end and print its scored summary",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			d, err := benchDir(dir)
			if err != nil {
				return err
			}
			o.BenchDir, o.Scenario, o.Progress = d, args[0], cmd.ErrOrStderr()
			if !cmd.Flags().Changed("out") {
				o.Out = filepath.Join(d, "results")
			}
			res, err := bench.Run(o)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), bench.Markdown(res.Report))
			if res.JSON != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "\nWrote %s and %s\n", res.JSON, res.MD)
			}
			if res.Dir != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Kept the world in %s\n", res.Dir)
			}
			return nil
		},
	}
	f := run.Flags()
	f.StringVar(&o.Adapter, "adapter", "fake", "agent: "+strings.Join(bench.Adapters, ", ")+" (only fake runs today)")
	f.StringVar(&o.Granularity, "granularity", "file", "claim granularity: "+strings.Join(bench.Granularities, ", ")+" (only file today)")
	f.StringVar(&o.Out, "out", "", "where to write the JSON and markdown (default: <bench dir>/results)")
	f.DurationVar(&o.Stall, "stall", bench.DefaultStall, "end the run after this long without progress")
	f.DurationVar(&o.Timeout, "timeout", bench.DefaultTimeout, "end the run regardless after this long")
	f.BoolVar(&o.Keep, "keep", false, "keep the world's temp dir and print its path")

	cmd.AddCommand(run, &cobra.Command{
		Use:   "score <run.json>",
		Short: "Rescore a saved run's event log and print the summary",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			var saved struct {
				Log bench.Log `json:"log"`
			}
			if err := json.Unmarshal(b, &saved); err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			fmt.Fprint(cmd.OutOrStdout(), bench.Markdown(bench.Score(saved.Log)))
			return nil
		},
	})
	return cmd
}

// benchDir is dir, else bench/ here, else bench/ at the checkout's top.
func benchDir(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	if _, err := os.Stat(filepath.Join("bench", "scenarios")); err == nil {
		return "bench", nil
	}
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err == nil {
		d := filepath.Join(strings.TrimSpace(string(out)), "bench")
		if _, err := os.Stat(filepath.Join(d, "scenarios")); err == nil {
			return d, nil
		}
	}
	return "", fmt.Errorf("no bench/scenarios here or at the top of this checkout; run from a saddle source checkout or pass --dir")
}
