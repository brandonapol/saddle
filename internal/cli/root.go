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
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/hook"
	"github.com/brandonapol/saddle/internal/mcpserver"
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
		&cobra.Command{
			Use:   "version",
			Short: "Print the saddle version",
			Run:   func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), Version) },
		},
		initCmd(), upCmd(), downCmd(), spawnCmd(), statusCmd(), claimCmd(), releaseCmd(), doneCmd(),
		landCmd(), syncCmd(), prsCmd(), killCmd(), messageCmd(), checkCmd(), hookCmd(), mcpCmd(), exitedCmd(), sweepCmd(),
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
	return &cobra.Command{
		Use:   "init",
		Short: "Create .saddle/ with a config template",
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			if err := a.Init(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "initialized %s/.saddle (edit .saddle/config.toml)\n", a.Root)
			return nil
		}),
	}
}

func upCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "up [epic-file|-]",
		Short: "Open the Saddle TUI: chat with the orchestrator, watch your agents",
		Long: `Opens Saddle's TUI. The orchestrator (a headless Claude Code session) lives in
the chat on the right. Tell it what to work on, e.g. "do #46 and #47 in parallel".
It starts agents in a hidden tmux session, watches them, and tells you when one
needs you. Quitting leaves the agents running; run saddle up again to come back.`,
		Args: cobra.MaximumNArgs(1),
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
			if err := a.Init(); err != nil {
				return err
			}
			lock, err := os.OpenFile(filepath.Join(a.Root, ".saddle", "tui.lock"), os.O_CREATE|os.O_RDWR, 0o644)
			if err != nil {
				return err
			}
			defer lock.Close()
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				return errors.New("saddle up is already running for this repo in another terminal")
			}
			if warn := a.LocalBaseBehind(); warn != "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+warn)
			}
			first := ""
			if len(args) == 1 {
				b, err := readArg(args[0])
				if err != nil {
					return err
				}
				first = "Here is an epic. Plan it and show me the plan.\n\n" + string(b)
			}
			if err := tui.Run(a, first); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Agents keep running in tmux session "+a.Cfg.Session+". Run `saddle up` to come back, `saddle down` to stop them.")
			return nil
		}),
	}
}

func downCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "down",
		Short: "Stop every agent (worktrees and branches are kept)",
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			n, err := a.Down()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "stopped %d agents\n", n)
			return nil
		}),
	}
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
		RunE: withApp(func(cmd *cobra.Command, a *app.App, args []string) error {
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
	f.BoolVar(&r.Force, "force", false, "ignore claim conflicts and the concurrency cap")
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
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "ID\tSTATUS\tMODEL\tWIN\tTITLE\tCLAIMS\tTRAIN")
			for _, t := range st.Tasks {
				status := t.Status
				if t.Notices > 0 {
					status += fmt.Sprintf(" (%d)", t.Notices)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, status, t.Model, t.Window, trunc(t.Title, 36),
					trunc(strings.Join(t.Claims, ","), 40), t.Train)
			}
			return w.Flush()
		}),
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
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
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
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
	return &cobra.Command{
		Use:   "prs",
		Short: "Push landed branches and open or update a stack of PRs",
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			urls, err := a.PRs()
			for _, u := range urls {
				fmt.Fprintln(cmd.OutOrStdout(), u)
			}
			return err
		}),
	}
}

func killCmd() *cobra.Command {
	var rm bool
	cmd := &cobra.Command{
		Use:   "kill <task>",
		Short: "Stop a task's agent and release its claims",
		Args:  cobra.ExactArgs(1),
		RunE: withApp(func(_ *cobra.Command, a *app.App, args []string) error {
			return a.Kill(args[0], rm)
		}),
	}
	cmd.Flags().BoolVar(&rm, "rm", false, "also remove the worktree (the branch is kept)")
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

func mcpCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "mcp",
		Short:  "Stdio MCP server for agents",
		Hidden: true,
		RunE: withApp(func(cmd *cobra.Command, a *app.App, _ []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
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
