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
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/engine"
	"github.com/brandonapol/saddle/internal/hook"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/spf13/cobra"
)

// pluginCmd holds the entrypoints of the saddle Claude Code plugin (plugin/
// in this repo), which lets the user's own Claude Code session be the
// orchestrator instead of the TUI's headless one.
func pluginCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plugin",
		Short: "Entrypoints for the saddle Claude Code plugin (orchestrate from your own Claude Code session)",
	}
	cmd.AddCommand(pluginMCPCmd(), pluginHookCmd(), pluginBriefCmd(), pluginEngineCmd(), pluginWaitCmd())
	return cmd
}

// openPlugin opens the repo containing dir for the plugin, or says why the
// plugin stays out of the way: the session is one of saddle's own agents
// (they have SADDLE_TASK), or the repo hasn't run saddle init. The plugin is
// enabled in every project, so it must never create .saddle/ by itself.
func openPlugin(dir string) (*app.App, string) {
	if os.Getenv("SADDLE_TASK") != "" {
		return nil, "This session is one of saddle's own agents; the orchestrator tools are off here."
	}
	root := os.Getenv("SADDLE_ROOT")
	if root == "" {
		root = saddleRoot(dir)
	}
	if root == "" {
		return nil, "Saddle isn't set up here. Run `saddle init` and `saddle doctor` in a git repo to orchestrate agents in it."
	}
	a, err := app.Open(root)
	if err != nil {
		return nil, "Saddle could not open this repo: " + err.Error()
	}
	return a, ""
}

// saddleRoot finds the nearest directory at or above dir that saddle init
// has set up, or "". It only stats files: the plugin's hook runs on every
// tool call in every project, so it can't afford to run git.
func saddleRoot(dir string) string {
	for dir = filepath.Clean(dir); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".saddle", "config.toml")); err == nil {
			return dir
		}
		if dir == filepath.Dir(dir) {
			return ""
		}
	}
}

func wd() string {
	d, _ := os.Getwd()
	return d
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func pluginMCPCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "mcp",
		Short:  "Stdio MCP server acting as the orchestrator",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signalContext()
			defer stop()
			a, why := openPlugin(wd())
			if a == nil {
				return mcpserver.ServeIdle(ctx, why)
			}
			defer a.Close()
			if _, err := a.EnsureOrchestrator(); err != nil {
				return mcpserver.ServeIdle(ctx, "Saddle could not set up the orchestrator: "+err.Error())
			}
			return mcpserver.Serve(ctx, a, app.OrchestratorID)
		},
	}
}

// pluginHookCmd delivers the orchestrator's notices into the session, but
// only while the plugin engine runs: that is what makes a session the
// orchestrator. Elsewhere, including in saddle's own agents and while saddle
// up owns the orchestrator, it does nothing. It fails open.
func pluginHookCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "hook",
		Short:  "Claude Code hook entrypoint for the orchestrating session",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := runPluginHook(cmd.InOrStdin(), cmd.OutOrStdout()); err != nil {
				fmt.Fprintln(cmd.ErrOrStderr(), "saddle plugin hook:", err)
			}
			return nil
		},
	}
}

func runPluginHook(r io.Reader, w io.Writer) error {
	var in hook.Input
	if err := json.NewDecoder(r).Decode(&in); err != nil {
		return err
	}
	dir := in.Cwd
	if dir == "" {
		dir = wd()
	}
	a, _ := openPlugin(dir)
	if a == nil {
		return nil
	}
	defer a.Close()
	if a.LockOwner() != app.LockEngine {
		return nil
	}
	out := hook.HandleOrchestrator(a, in)
	if out == nil {
		return nil
	}
	return json.NewEncoder(w).Encode(out)
}

func pluginBriefCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "brief",
		Short: "Print the orchestrator brief and where things stand",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, why := openPlugin(wd())
			if a == nil {
				fmt.Fprintln(cmd.OutOrStdout(), why)
				return nil
			}
			defer a.Close()
			if _, err := a.EnsureOrchestrator(); err != nil {
				return err
			}
			return writeBrief(cmd.OutOrStdout(), a)
		},
	}
}

func writeBrief(w io.Writer, a *app.App) error {
	fmt.Fprintln(w, a.PluginBrief())
	fmt.Fprintln(w, "## Right now")
	switch a.LockOwner() {
	case app.LockEngine:
		fmt.Fprintln(w, "- Engine: running.")
	case app.LockUp:
		fmt.Fprintln(w, "- ‼ saddle up is running in another terminal, and its TUI orchestrator gets the agents' events. Quit it before orchestrating from here (agents keep running), or use the TUI.")
	default:
		fmt.Fprintln(w, "- Engine: not running. Start `saddle plugin engine` in the background before spawning.")
	}
	ts, err := mcpserver.Tasks(a)
	if err != nil {
		return err
	}
	n := 0
	for _, t := range ts {
		if t.ID == app.OrchestratorID || !activeStatus(t.Status) {
			continue
		}
		if n == 0 {
			fmt.Fprintln(w, "- Agents:")
		}
		n++
		line := fmt.Sprintf("  - %s (%s): %s", t.ID, t.Title, t.Status)
		if t.Train != "" {
			line += ", train " + t.Train
		}
		fmt.Fprintln(w, line)
	}
	if n == 0 {
		fmt.Fprintln(w, "- No agents are working.")
	}
	return nil
}

func activeStatus(s string) bool {
	switch s {
	case store.Killed, store.Landed:
		return false
	}
	return true
}

func pluginEngineCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "engine",
		Short: "Watch agents for the orchestrating Claude Code session (runs until stopped)",
		Long: `Runs what saddle up runs in the background, without the TUI: it notices
agents waiting on a prompt or stopped without calling done and queues those
events for the orchestrator, and runs the stack sentinel, CI watcher and
auto-merge watcher. While it runs, the plugin's hooks deliver the events into
this repo's orchestrating Claude Code session. It can't run alongside saddle up.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, why := openPlugin(wd())
			if a == nil {
				return errors.New(why)
			}
			defer a.Close()
			if _, err := a.EnsureOrchestrator(); err != nil {
				return err
			}
			release, err := a.AcquireLock(app.LockEngine)
			if err != nil {
				return err
			}
			defer release()
			ctx, stop := signalContext()
			defer stop()
			fmt.Fprintf(cmd.OutOrStdout(), "saddle engine running for %s. Stop it with ctrl-c; agents keep running.\n", a.Root)
			stopWatchers := startWatchers(ctx, a)
			err = engine.New(a).Run(ctx)
			stopWatchers()
			return err
		},
	}
}

func pluginWaitCmd() *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait",
		Short: "Block until an event needs the orchestrator, then print every pending event",
		Long: `Run it in the background from the orchestrating session: it exits, and so
wakes the session, as soon as an agent needs attention, conflicts, fails or
CI breaks. Routine events (spawned, landed) are printed along with it.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, why := openPlugin(wd())
			if a == nil {
				return errors.New(why)
			}
			defer a.Close()
			ctx, stop := signalContext()
			defer stop()
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			return waitNotices(ctx, a, time.Second, cmd.OutOrStdout())
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up after this long (0 waits until an event)")
	return cmd
}

// waitNotices polls the orchestrator's notices every poll until an action
// notice is pending, then takes and prints them all. When ctx ends first it
// says so and exits cleanly, so the session can start another wait.
func waitNotices(ctx context.Context, a *app.App, poll time.Duration, w io.Writer) error {
	var warn string
	if a.LockOwner() != app.LockEngine {
		warn = "warning: saddle plugin engine is not running, so agents stuck on a prompt won't be reported. Start it in the background.\n\n"
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		n, err := a.Store.PendingActionNotices(app.OrchestratorID)
		if err != nil {
			return err
		}
		if n > 0 {
			ns, err := a.Store.TakeNotices(app.OrchestratorID, false)
			if err != nil {
				return err
			}
			fmt.Fprint(w, warn)
			fmt.Fprintln(w, strings.TrimSpace(store.FormatNotices(ns)))
			return nil
		}
		select {
		case <-ctx.Done():
			fmt.Fprint(w, warn)
			fmt.Fprintln(w, "No saddle events need you yet. Start another wait while agents are working.")
			return nil
		case <-t.C:
		}
	}
}
