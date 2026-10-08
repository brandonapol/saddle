package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/spf13/cobra"
)

// autopilotCmd steers the autopilot driver (#256).
func autopilotCmd() *cobra.Command {
	var asJSON bool
	status := func(cmd *cobra.Command, a *app.App, _ []string) error {
		st, err := a.AutopilotState()
		if err != nil {
			return err
		}
		if asJSON {
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(st)
		}
		writeAutopilot(cmd.OutOrStdout(), st)
		return nil
	}
	cmd := &cobra.Command{
		Use:   "autopilot",
		Short: "Let saddle drive the loop: land, top up from ready issues, nudge on stalls, until a stop condition",
		Long: `While saddle up runs and autopilot is on, saddle itself keeps the pipeline
moving every 30 seconds: it resumes tasks that lost their agent, lands what
is queued and publishes PRs, and tops up to the concurrency cap with open
issues labelled ready (oldest first), skipping ones with an open PR or a
task, waiting on 'after: #N' lines, and starting only issues whose
'claims:' globs are disjoint from everything running (an issue that
declares none runs alone). The model comes from the ticket's
'Model scope:' line, Opus unless it says Sonnet. Under plan-limit pressure
it stops spawning and sleeps until the reset. When ready work can't start
or notices go unread, it nudges an idle orchestrator once.

A stop condition stops new spawns; the run ends once its tasks are done,
with a summary event and a notice. Merging stays with 'saddle automerge'.`,
		RunE: withApp(status),
	}
	cmd.PersistentFlags().BoolVar(&asJSON, "json", false, "print JSON")

	var until, untilUsage, label string
	var maxTasks int
	on := &cobra.Command{
		Use:   "on",
		Short: "Start a run: top up from the ready queue until the stop condition",
		Args:  cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			var o autopilot.Options
			var err error
			if until != "" {
				if o.Stop.Until, err = autopilot.ParseUntil(until, time.Now()); err != nil {
					return err
				}
			}
			if untilUsage != "" {
				if o.Stop.UntilUsage, err = autopilot.ParseUsage(untilUsage); err != nil {
					return err
				}
			}
			if maxTasks < 0 {
				return fmt.Errorf("--max-tasks %d: want 0 (no limit) or more", maxTasks)
			}
			o.Stop.MaxTasks, o.ReadyLabel = maxTasks, label
			st, err := a.NewAutopilot(nil).Enable(o)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "autopilot is on: issues labelled %s, %s; it runs while saddle up does\n", st.Label(), st.Stop)
			return nil
		}),
	}
	f := on.Flags()
	f.StringVar(&until, "until", "", "stop spawning at this time (HH:MM, or RFC 3339)")
	f.StringVar(&untilUsage, "until-usage", "", "stop spawning at this share of a plan-limit window (e.g. 90%)")
	f.IntVar(&maxTasks, "max-tasks", 0, "stop after spawning this many tasks (0: no limit)")
	f.StringVar(&label, "ready-label", autopilot.DefaultReadyLabel, "label of the issues autopilot may pick up")

	cmd.AddCommand(on, &cobra.Command{
		Use:   "infinite on|off",
		Short: "Infinite mode: run until the plan limit, park there, resume on reset (the TUI's alt+i)",
		Long: `Infinite mode is autopilot with no stop condition. When the ready queue
is empty it asks the orchestrator to find more work instead of ending the
run. When a plan-limit window is full it parks the running tasks and
resumes them once the window resets. Holds, ci-red and the sentinels still
apply. On over a running bounded run keeps that run; off ends the run.`,
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			if args[0] != "on" && args[0] != "off" {
				return fmt.Errorf("infinite %q: want on or off", args[0])
			}
			st, err := a.NewAutopilot(nil).SetInfinite(args[0] == "on")
			switch {
			case err != nil:
				return err
			case st.Infinite:
				fmt.Fprintf(cmd.OutOrStdout(), "infinite mode is on: issues labelled %s, %s; it runs while saddle up does\n", st.Label(), st.Goal())
			case st.Summary != "" && !st.On:
				fmt.Fprintln(cmd.OutOrStdout(), st.Summary)
			default:
				fmt.Fprintln(cmd.OutOrStdout(), "infinite mode is off")
			}
			return nil
		}),
	}, &cobra.Command{
		Use:   "off",
		Short: "End the run now and write its summary; running tasks go on",
		Args:  cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			st, err := a.NewAutopilot(nil).Disable()
			if err != nil {
				return err
			}
			if st.Summary == "" {
				fmt.Fprintln(cmd.OutOrStdout(), "autopilot is off")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), st.Summary)
			return nil
		}),
	}, &cobra.Command{
		Use:   "pause",
		Short: "Hold the driver: no landing, spawning or nudges until resume",
		Args:  cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			if _, err := a.NewAutopilot(nil).Pause(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "autopilot is paused; `saddle autopilot resume` continues the run")
			return nil
		}),
	}, &cobra.Command{
		Use:   "resume",
		Short: "Continue a paused run",
		Args:  cobra.NoArgs,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			if _, err := a.NewAutopilot(nil).Resume(); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "autopilot resumed")
			return nil
		}),
	}, &cobra.Command{
		Use:   "status",
		Short: "Show the run, its stop condition and the last decision",
		Args:  cobra.NoArgs,
		RunE:  withApp(status),
	})
	return cmd
}

func writeAutopilot(out io.Writer, st autopilot.State) {
	hm := func(t time.Time) string { return t.Local().Format("15:04") }
	if !st.On {
		fmt.Fprintln(out, "autopilot: off")
		if st.Summary != "" {
			fmt.Fprintln(out, "last run: "+st.Summary)
		}
		return
	}
	state := "on"
	if st.Infinite {
		state = "∞ on"
	}
	if st.Paused {
		state = "paused"
	}
	fmt.Fprintf(out, "autopilot: %s (%s), ready label %s, since %s\n", state, st.Goal(), st.Label(), hm(st.Started))
	if len(st.Spawned) > 0 {
		var ids []string
		for _, s := range st.Spawned {
			ids = append(ids, fmt.Sprintf("%s (#%d)", s.Task, s.Issue))
		}
		fmt.Fprintf(out, "spawned %d: %s\n", len(ids), strings.Join(ids, ", "))
	}
	if st.Draining != "" {
		fmt.Fprintf(out, "draining: %s; it ends once its tasks are done\n", st.Draining)
	}
	if !st.SleepUntil.IsZero() {
		fmt.Fprintf(out, "sleeping until %s for plan limits\n", hm(st.SleepUntil))
	}
	if len(st.Parked) > 0 {
		fmt.Fprintf(out, "parked until the reset: %s\n", strings.Join(st.Parked, ", "))
	}
	if st.LastTick.IsZero() {
		fmt.Fprintln(out, "no tick yet: it runs while saddle up does")
		return
	}
	fmt.Fprintf(out, "last tick %s: %s\n", hm(st.LastTick), st.LastDecision)
}
