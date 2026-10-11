// Package cli wires saddle's cobra commands.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"text/tabwriter"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/banner"
	"github.com/brandonapol/saddle/internal/ciwatch"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/doctor"
	"github.com/brandonapol/saddle/internal/hook"
	"github.com/brandonapol/saddle/internal/initcmd"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/refguard"
	"github.com/brandonapol/saddle/internal/remote"
	"github.com/brandonapol/saddle/internal/sentinel"
	"github.com/brandonapol/saddle/internal/tui"
	"github.com/spf13/cobra"
)

// Version is set at build time with -ldflags "-X github.com/brandonapol/saddle/internal/cli.Version=...".
var Version = "dev"

func Root() *cobra.Command {
	root := &cobra.Command{
		Use:          "saddle",
		Short:        "Ride a herd of coding agents from one terminal",
		SilenceUsage: true,
	}
	root.AddCommand(
		restackCmd(),
		versionCmd(), upgradeCmd(), migrateCmd(), changelogCmd(), initCmd(), upCmd(), downCmd(), spawnCmd(), withTmux(statusCmd()), briefCmd(), claimCmd(), releaseCmd(), doneCmd(),
		landCmd(), syncCmd(), prsCmd(), killCmd(), gcCmd(), messageCmd(), checkCmd(), hookCmd(), mcpCmd(), exitedCmd(), sweepCmd(), refguardCmd(), perfCmd(), unstackCmd(), sentinelCmd(), requeueCmd(), queueCmd(), planCmd(), doctorCmd(), automergeCmd(), stackCmd(), repairCmd(), concurrencyCmd(), pluginCmd(), grokBridgeCmd(), publishCmd(), noticesCmd(), trustCmd(), untrustCmd(), resumeCmd(), rescueCmd(), heavyRunCmd(), runqCmd(), autopilotCmd(), remote.Command(open),
	)
	return root
}

func open() (*app.App, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return app.Open(wd)
}

// withApp opens the repo state for the duration of a command.
func withApp(fn func(cmd *cobra.Command, a *app.App, args []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		a, err := open()
		if err != nil {
			return err
		}
		defer a.Close()
		return fn(cmd, a, args)
	}
}

// withScratch is withApp for the commands that start gates, the train or
// agents: it points TMPDIR and GOTMPDIR at the scratch root first, so all
// they start writes its temp files there, not /tmp (#322).
func withScratch(fn func(cmd *cobra.Command, a *app.App, args []string) error) func(*cobra.Command, []string) error {
	return withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
		if err := a.UseScratch(); err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), "warning: scratch root: "+err.Error())
		}
		return fn(cmd, a, args)
	})
}

// resolveTask picks the task from --task, then SADDLE_TASK, then the current worktree.
func resolveTask(a *app.App, flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if t := os.Getenv("SADDLE_TASK"); t != "" {
		return t, nil
	}
	wd, _ := os.Getwd()
	t, err := a.TaskForDir(wd)
	if err != nil {
		return "", fmt.Errorf("no task given (use --task, or run inside a task worktree): %w", err)
	}
	return t.ID, nil
}

func initCmd() *cobra.Command {
	var quiet, trusted bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create .saddle/ with a config template",
		Long: `Creates .saddle/ with a config template and installs saddle's git hooks.
The first time in a repo it asks whether you trust it, before writing anything.
Without a terminal, pass --trust (or set SADDLE_TRUST=1).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			banner.Print(cmd.OutOrStdout(), banner.Options{Quiet: quiet, IsTTY: stdoutIsTTY})
			if err := gateTrust(cmd.OutOrStdout(), cmd.InOrStdin(), trusted); err != nil {
				return err
			}
			return withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
				if err := a.Init(); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "initialized %s/.saddle (edit .saddle/config.toml)\n", a.Root)
				return nil
			})(cmd, args)
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "don't print the howdy banner")
	cmd.Flags().BoolVar(&trusted, "trust", false, "trust this repo without asking (remembered; see saddle trust)")
	return cmd
}

func upCmd() *cobra.Command {
	var skipDoctor, trusted bool
	cmd := &cobra.Command{
		Use:   "up [claude|grok|codex] [epic-file|-]",
		Short: "Open the Saddle TUI: chat with the orchestrator, watch your agents",
		Long: `Opens Saddle's TUI. The orchestrator lives in
the chat on the right (Claude Code, or the Grok CLI when harness = "grok").
Tell it what to work on, e.g. "do #46 and #47 in parallel".
It starts agents in a hidden tmux session, watches them, and tells you when one
needs you. Quitting leaves the agents running; run saddle up again to come back.

Name an agent to use it for this run only: the orchestrator and every
worker it spawns without an explicit adapter. Config is not changed; the
next bare saddle up uses harness from config again. Agents already running
keep the CLI they were spawned with.

  saddle up              harness from config (default claude)
  saddle up grok         Grok CLI for this run
  saddle up codex epic.md
  saddle up ./grok       a file named grok, read as an epic`,
		Args: cobra.MaximumNArgs(2),
		PreRunE: func(cmd *cobra.Command, args []string) error {
			harness, _, err := parseUpArgs(args)
			if err != nil {
				return err
			}
			if err := applyUpHarness(harness); err != nil {
				return err
			}
			return upTrustErr(gateTrust(cmd.OutOrStdout(), cmd.InOrStdin(), trusted))
		},
		RunE: withScratch(func(cmd *cobra.Command, a *app.App, args []string) error {
			// Local setup first (#163), so the doctor only stops up for what
			// needs the user. PreRunE already checked the repo is trusted.
			steps, err := initcmd.Ensure(a.Root, a.Init)
			if err != nil {
				return err
			}
			initcmd.Report(cmd.OutOrStdout(), steps)
			if err := upDoctor(cmd.OutOrStdout(), skipDoctor, upDoctorTimeout, func() []doctor.Result {
				return doctor.Run(doctor.System(a.Root))
			}); err != nil {
				return err
			}
			if err := a.CheckMergeSettings(cmd.ErrOrStderr()); err != nil {
				return err
			}
			release, err := a.AcquireLock(app.LockUp)
			if err != nil {
				return err
			}
			defer release()
			defer recordUp(a.Root)()              // lets other commands spot a stale saddle up (#326)
			a.CleanStaleLeases(cmd.ErrOrStderr()) // heavy-run leases a crash left behind (#238)
			if warn := a.LocalBaseBehind(); warn != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+warn)
			}
			first := ""
			if _, epic, _ := parseUpArgs(args); epic != "" {
				b, err := readArg(epic)
				if err != nil {
					return err
				}
				first = "Here is an epic. Plan it and show me the plan.\n\n" + string(b)
			}
			term := make(chan os.Signal, 1)
			signal.Notify(term, syscall.SIGTERM) // Bubble Tea quits on it too
			defer signal.Stop(term)
			stop := startWatchers(cmd.Context(), a)
			err = tui.Run(a, first)
			stop()
			if paused, perr := pauseOnTerm(term, a); perr != nil || paused != nil {
				writePaused(cmd.OutOrStdout(), paused)
				return errors.Join(err, perr)
			}
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Agents keep running in tmux session "+a.Cfg.Session+". Run `saddle up` to come back, `saddle down` to stop them.")
			return nil
		}),
	}
	cmd.Flags().BoolVar(&skipDoctor, "skip-doctor", false, "start without running the doctor checks")
	cmd.Flags().BoolVar(&trusted, "trust", false, "trust this repo without asking (remembered; see saddle trust)")
	return cmd
}

// startWatchers starts the background loops that live as long as saddle up:
// the stack sentinel, the auto-merge watcher (which merges nothing unless
// on), the orchestrator compact watcher, the notice digest, the resume
// watcher, the checkpoint watcher, the scratch sweeper and, unless
// ci.disabled, the CI and
// ci-red watchers. Short-lived commands never
// start them. The returned func stops them and waits until they have.
func startWatchers(ctx context.Context, a *app.App) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = sentinel.New(a).Run(ctx) // Run records failed checks as events
	})
	wg.Go(func() { _ = a.NewAutomerge(nil).Run(ctx) })   // merges only when on; failures are events
	wg.Go(func() { _ = a.NewCompactWatcher().Run(ctx) }) // notices and compacts; failures are events
	wg.Go(func() { a.RunDigest(ctx) })                   // routine notices as one digest line; failures are events
	wg.Go(func() { a.RunResumeWatcher(ctx) })            // new windows for tasks that lost theirs (#254); failures are events
	wg.Go(func() { _ = a.NewAutopilot(nil).Run(ctx) })   // drives the loop only while autopilot is on; failures are events
	wg.Go(func() { a.NewCheckpointWatcher().Run(ctx) })  // checkpoints uncommitted work and nudges commits (#50); failures are events
	wg.Go(func() { a.RunScratchSweeper(ctx) })           // sweeps saddle's scratch now and every 10m, holds spawns when low (#322)
	if !a.Cfg.CI.Disabled {
		wg.Go(func() { _ = sentinel.NewCIRed(a).Run(ctx) }) // holds layers above red CI; errors are events
		if ci, err := a.NewCIWatcher(ciwatch.ExecRunner(a.Root)); err == nil {
			wg.Go(func() { ci.Run(ctx) }) // gh errors are recorded as events
		}
	}
	return func() {
		cancel()
		wg.Wait()
	}
}

func downCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Stop every agent; running tasks are paused and saddle up resumes them",
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			paused, err := a.Down()
			if err != nil {
				return err
			}
			writePaused(cmd.OutOrStdout(), paused)
			left, _, err := a.GCCounts()
			if err != nil {
				return err
			}
			if left > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "%d leftover worktrees, branches or refs; run `saddle gc --dry-run` to list them, `saddle gc` to remove them\n", left)
			}
			return nil
		}),
	}
}

// parseUpArgs splits saddle up's arguments into an agent name and an epic
// file (or "-"). A known agent name is always the agent, even when a file of
// that name exists; ./name reads the file.
func parseUpArgs(args []string) (harness, epic string, err error) {
	if len(args) > 0 && slices.Contains(config.Harnesses(), args[0]) {
		harness, args = args[0], args[1:]
	}
	switch {
	case len(args) == 0:
		return harness, "", nil
	case len(args) > 1:
		return "", "", fmt.Errorf("saddle up takes an agent (%s) and one epic file or -", strings.Join(config.Harnesses(), ", "))
	case args[0] == "-":
		return harness, "-", nil
	}
	if _, err := os.Stat(args[0]); err != nil {
		return "", "", fmt.Errorf("%q is not an agent (%s) or a readable epic file", args[0], strings.Join(config.Harnesses(), ", "))
	}
	return harness, args[0], nil
}

// applyUpHarness makes harness this process tree's agent: config.Load reads
// it, and launches pass it on to the orchestrator's MCP server and workers.
func applyUpHarness(harness string) error {
	if harness == "" {
		return nil
	}
	return os.Setenv(config.HarnessEnv, harness)
}

func readArg(arg string) ([]byte, error) {
	if arg == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(arg)
}

func spawnCmd() *cobra.Command {
	var r app.SpawnReq
	var promptFile string
	cmd := &cobra.Command{
		Use:   "spawn <title> [prompt]",
		Short: "Start an agent on its own branch, worktree and tmux window",
		Args:  cobra.RangeArgs(1, 2),
		RunE: withScratch(func(cmd *cobra.Command, a *app.App, args []string) error {
			r.Title = args[0]
			if len(args) == 2 {
				r.Prompt = args[1]
			}
			if promptFile != "" {
				b, err := readArg(promptFile)
				if err != nil {
					return err
				}
				r.Prompt = string(b)
			}
			if r.Prompt == "" {
				r.Prompt = r.Title
			}
			if r.Parent == "" {
				r.Parent = os.Getenv("SADDLE_TASK")
			}
			t, err := a.Spawn(r)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s started: branch %s, window %s\n", t.ID, t.Branch, t.Window)
			return nil
		}),
	}
	f := cmd.Flags()
	f.StringVar(&r.ID, "id", "", "task id (default: next tN)")
	f.StringSliceVarP(&r.Claims, "claim", "c", nil, "path glob this task owns (repeatable)")
	f.StringVarP(&r.Model, "model", "m", "", "claude model (default from config)")
	f.StringVar(&r.Parent, "parent", "", "parent task id")
	f.StringVar(&r.Base, "base", "", "base ref (default: integration branch)")
	f.StringVarP(&promptFile, "prompt-file", "f", "", "read the prompt from a file (- for stdin)")
	f.BoolVar(&r.Force, "force", false, "ignore claim conflicts, the concurrency cap and heavy-run queue backpressure")
	return cmd
}

func statusCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "status",
		Aliases: []string{"ls"},
		Short:   "List tasks, claims and the merge train",
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			st, err := mcpserver.Status(a)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(st)
			}
			for _, warn := range st.Warnings {
				fmt.Fprintln(cmd.OutOrStdout(), "warning: "+warn)
			}
			if r := st.StackAtRisk; r != nil {
				ack := ""
				if r.Acked {
					ack = " (acknowledged)"
				}
				fmt.Fprintf(cmd.OutOrStdout(), "stack at risk from %s up%s: %s\n", r.Task, ack, r.Cause)
				if len(r.PRs) > 0 {
					fmt.Fprintln(cmd.OutOrStdout(), "  labeled needs-human: "+strings.Join(r.PRs, ", "))
				}
				fmt.Fprintln(cmd.OutOrStdout(), "  "+r.Fix)
			}
			if holds, err := a.CIRedHolds(); err == nil {
				writeCIRed(cmd.OutOrStdout(), holds)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tSTATUS\tMODEL\tWIN\tTITLE\tCLAIMS\tTRAIN")
			for _, t := range st.Tasks {
				status := t.Status
				if t.Notices > 0 {
					status += fmt.Sprintf(" (%d)", t.Notices)
				}
				if t.Reason != "" {
					status += ": " + trunc(t.Reason, 60)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, status, t.Model, t.Window, trunc(t.Title, 36),
					trunc(strings.Join(t.Claims, ","), 40), t.Train)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			writeAdapters(cmd.OutOrStdout(), st.Adapters)
			if st.HeavyRuns != nil {
				writeHeavyRuns(cmd.OutOrStdout(), *st.HeavyRuns)
			}
			writeHeavyStatsLine(cmd.OutOrStdout(), a)
			return nil
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// writeAdapters prints which coding CLIs a worker can run on (#182).
func writeAdapters(out io.Writer, ss []agent.Status) {
	if len(ss) == 0 {
		return
	}
	line := "adapters: " + strings.Join(agent.Usable(ss), ", ")
	for _, s := range ss {
		if !s.OK {
			line += fmt.Sprintf("; %s unavailable (%s)", s.Name, s.Reason)
		}
	}
	fmt.Fprintln(out, line)
}

// writeCIRed prints each red layer and what it holds back (#213).
func writeCIRed(out io.Writer, holds []app.CIRedHold) {
	for _, h := range holds {
		ack := ""
		if h.Acked {
			ack = " (acknowledged)"
		}
		fmt.Fprintf(out, "ci-red on %s%s: %s on %s\n", h.Task, ack, strings.Join(h.Checks, ", "), h.PR)
		if len(h.Held) > 0 {
			fmt.Fprintln(out, "  holds the layers above it: "+strings.Join(h.Held, ", "))
		}
		if len(h.Queued) > 0 {
			fmt.Fprintln(out, "  holds queued work that would stack on it: "+strings.Join(h.Queued, ", "))
		}
		switch {
		case h.Escalated:
			fmt.Fprintf(out, "  %d repairs failed; the orchestrator decides what next\n", h.Attempts)
		case h.Repair != "":
			fmt.Fprintf(out, "  %s is repairing it (attempt %d)\n", h.Repair, h.Attempts)
		}
	}
}

func trunc(s string, n int) string {
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n-1]) + "…"
}

func claimCmd() *cobra.Command {
	var task string
	cmd := &cobra.Command{
		Use:   "claim <glob>...",
		Short: "Reserve paths for a task",
		Args:  cobra.MinimumNArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			id, err := resolveTask(a, task)
			if err != nil {
				return err
			}
			r, err := a.Claim(id, args)
			if err != nil {
				return err
			}
			for _, g := range r.Granted {
				fmt.Fprintf(cmd.OutOrStdout(), "granted %s\n", g)
			}
			for g, owner := range r.Denied {
				fmt.Fprintf(cmd.OutOrStdout(), "denied  %s (held by %s)\n", g, owner)
			}
			if len(r.Denied) > 0 {
				return errors.New("some claims were denied")
			}
			return nil
		}),
	}
	cmd.Flags().StringVar(&task, "task", "", "task id")
	return cmd
}

func releaseCmd() *cobra.Command {
	var task string
	cmd := &cobra.Command{
		Use:   "release [glob]...",
		Short: "Release a task's claims (all of them if none are given)",
		RunE: withApp(func(_ *cobra.Command, a *app.App, args []string) error {
			id, err := resolveTask(a, task)
			if err != nil {
				return err
			}
			return a.Store.Release(id, args...)
		}),
	}
	cmd.Flags().StringVar(&task, "task", "", "task id")
	return cmd
}

func doneCmd() *cobra.Command {
	var task, summary string
	cmd := &cobra.Command{
		Use:   "done",
		Short: "Mark a task finished and queue it in the merge train",
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			id, err := resolveTask(a, task)
			if err != nil {
				return err
			}
			if err := a.Done(id, summary); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s queued\n", id)
			return nil
		}),
	}
	cmd.Flags().StringVar(&task, "task", "", "task id")
	cmd.Flags().StringVarP(&summary, "summary", "s", "", "what changed (becomes the PR description)")
	return cmd
}

func landCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "land",
		Short: "Run the merge train: land queued branches one at a time",
		RunE: withScratch(func(cmd *cobra.Command, a *app.App, _ []string) error {
			if err := a.CheckMergeSettings(cmd.ErrOrStderr()); err != nil {
				return err
			}
			rs, err := a.Land()
			for _, r := range rs {
				fmt.Fprintf(cmd.OutOrStdout(), "%-6s %-12s %s\n", r.Task, r.State, r.Note)
			}
			if err == nil && len(rs) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "train is empty")
			}
			return err
		}),
	}
}

func syncCmd() *cobra.Command {
	var task string
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Rebase this task's branch onto the integration branch (follows directory moves)",
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			id, err := resolveTask(a, task)
			if err != nil {
				return err
			}
			rr, err := a.Sync(id)
			if err != nil {
				return err
			}
			if rr.OK {
				fmt.Fprintf(cmd.OutOrStdout(), "synced onto %s\n", a.Cfg.Integration)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "rebase stopped with conflicts in:\n  %s\nResolve them, `git add` the files, then `git rebase --continue`.\n",
				strings.Join(rr.Conflicts, "\n  "))
			return errors.New("conflicts")
		}),
	}
	cmd.Flags().StringVar(&task, "task", "", "task id")
	return cmd
}

func prsCmd() *cobra.Command {
	var dryRun, pushOnly, gateOnly bool
	cmd := &cobra.Command{
		Use:   "prs",
		Short: "Push landed branches and open or update a stack of PRs",
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			if dryRun {
				plan, err := a.PreviewPRs()
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), plan.Note)
				for _, layer := range plan.Layers {
					fmt.Fprintf(cmd.OutOrStdout(), "stack %d: %s (%s) -> %s; head %s; re-cut=%t\n", layer.Group+1, layer.Task, layer.Branch, layer.Base, short(layer.Head), layer.Recut)
				}
				for _, check := range plan.Checks {
					fmt.Fprintf(cmd.OutOrStdout(), "gate %s: %s\n", check.Name, check.Cmd)
				}
				return nil
			}
			if gateOnly {
				_, err := a.PRsWithOptions(app.PRsOptions{GateOnly: true})
				if err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "pre-publish gate passed; no branches pushed or PRs changed")
				return nil
			}
			if err := a.CheckMergeSettings(cmd.ErrOrStderr()); err != nil {
				return err
			}
			urls, err := a.PRsWithOptions(app.PRsOptions{PushOnly: pushOnly})
			for _, u := range urls {
				fmt.Fprintln(cmd.OutOrStdout(), u)
			}
			return err
		}),
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview stack layout and gate commands without ref, state, remote or PR changes")
	cmd.Flags().BoolVar(&pushOnly, "push-only", false, "check and push branches without creating or updating PRs")
	cmd.Flags().BoolVar(&gateOnly, "gate-only", false, "run pre-publish checks without restacking or pushing")
	cmd.MarkFlagsMutuallyExclusive("dry-run", "push-only", "gate-only")
	return cmd
}

func messageCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "message <task> <text>",
		Short: "Send a [saddle] message to a task, waking it if idle",
		Args:  cobra.ExactArgs(2),
		RunE: withApp(func(_ *cobra.Command, a *app.App, args []string) error {
			return a.Notify(args[0], "action", "Message from the user: "+args[1])
		}),
	}
}

func checkCmd() *cobra.Command {
	var task string
	cmd := &cobra.Command{
		Use:    "check <file>",
		Short:  "Show whether a task may write a file (claims debugging)",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			id, err := resolveTask(a, task)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), hook.Describe(a.CheckWrite(id, args[0])))
			return nil
		}),
	}
	cmd.Flags().StringVar(&task, "task", "", "task id")
	return cmd
}

func grokBridgeCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "grok-bridge <run-dir>",
		Short:  "Headless Grok session that speaks Claude stream-json on stdin",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return agent.RunGrokBridge(args[0], cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

func hookCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "hook",
		Short:  "Claude Code hook entrypoint (reads hook JSON on stdin)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			task := os.Getenv("SADDLE_TASK")
			if task == "" {
				return nil
			}
			a, err := open()
			if err != nil {
				return nil // fail open
			}
			defer a.Close()
			if err := hook.Run(a, task, cmd.InOrStdin(), cmd.OutOrStdout()); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "saddle hook:", err)
			}
			return nil
		},
	}
}

// refguardCmd is the entrypoint of the reference-transaction hook and, with
// state "pre-push", of the pre-push hook. An error exits non-zero, which
// aborts the transaction or push. Hook finds the repo itself. The pre-push
// hook reuses this command rather than adding its own so that Go test
// binaries standing in for saddle answer it with the same `refguard <state>`.
func refguardCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "refguard <state|pre-push>",
		Long:   "Git hook entrypoint. For operator recovery outside a Saddle task, run SADDLE_REFGUARD=off git ... . Each guarded move or push is logged as operator in .saddle/state.db; repository hooks still run.",
		Short:  "Git reference-transaction and pre-push hook guarding saddle's branches",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return refguard.Hook(args[0], cmd.InOrStdin(), os.Getenv)
		},
	}
}

func mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "mcp",
		Short:  "Stdio MCP server for agents",
		Hidden: true,
		RunE: withScratch(func(cmd *cobra.Command, a *app.App, _ []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go a.ReloadOnSIGHUP(ctx) // the train's config sections; land reloads them too (#228)
			return mcpserver.Serve(ctx, a, os.Getenv("SADDLE_TASK"))
		}),
	}
}

func exitedCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "exited <task>",
		Short:  "Record that a task's agent process exited",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: withApp(func(_ *cobra.Command, a *app.App, args []string) error {
			t, err := a.Store.Task(args[0])
			if err != nil {
				return err
			}
			a.Store.Event(t.ID, "exited", "")
			if t.Status == "running" || t.Status == "needs_you" {
				return a.Store.SetStatus(t.ID, "idle")
			}
			return nil
		}),
	}
}
