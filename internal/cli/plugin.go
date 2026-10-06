package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/banner"
	"github.com/brandonapol/saddle/internal/doctor"
	"github.com/brandonapol/saddle/internal/engine"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/hook"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/trust"
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
	cmd.AddCommand(pluginMCPCmd(), pluginHookCmd(), pluginSetupCmd(), pluginInstallCmd(), pluginBriefCmd(), pluginEngineCmd(), pluginWaitCmd())
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
		return nil, "Saddle isn't set up here. Use /saddle:orchestrate in a git repo to set it up (it runs `saddle init` and `saddle doctor`), then restart the session to get the orchestrator tools."
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

// onboardMarker, under .saddle/, records that the plugin set this repo up:
// "pending" until the doctor passes, then "ok".
const onboardMarker = "plugin-onboarded"

// onboard sets saddle up the first time a plugin command is used in a repo
// that never ran saddle init. First, in any repo saddle isn't trusted in, it
// prints the trust prompt for the session to ask in chat and stops (see
// writePluginTrust). Then it greets the user, runs init and the doctor,
// and shows the doctor table. It reports whether the command may go on: only
// when no check fails (warnings are shown, not blocking). While a check
// fails, each later use reruns just the doctor. Repos that were already
// initialized, saddle's own agents and dirs outside git are left alone.
// Only explicit plugin commands call it, never the hook or MCP server, which
// run in every project.
func onboard(w io.Writer, dir string, runDoctor func(root string) []doctor.Result) bool {
	if os.Getenv("SADDLE_TASK") != "" {
		return true
	}
	root := os.Getenv("SADDLE_ROOT")
	if root == "" {
		root = saddleRoot(dir)
	}
	first := root == ""
	if first {
		var err error
		if root, err = gitx.Root(dir); err != nil {
			return true // openPlugin explains that saddle needs a git repo
		}
	}
	if err := trust.Gate(root, trust.Options{}); err != nil {
		if first {
			fmt.Fprint(w, banner.Howdy())
			fmt.Fprintln(w)
		}
		writePluginTrust(w, root, err)
		return false
	}
	if !first {
		b, _ := os.ReadFile(filepath.Join(root, ".saddle", onboardMarker))
		if strings.TrimSpace(string(b)) != "pending" {
			return true
		}
	} else {
		fmt.Fprint(w, banner.Howdy()) // captured by Claude Code: never colored
		fmt.Fprintf(w, "\nFirst use of saddle in %s: running saddle init and saddle doctor.\n\n", root)
		if err := initRepo(root); err != nil {
			fmt.Fprintln(w, "saddle init failed:", err)
			return false
		}
		if err := writeMarker(root, "pending"); err != nil {
			fmt.Fprintln(w, "saddle init:", err)
			return false
		}
	}
	rs := runDoctor(root)
	doctor.WriteTable(w, rs)
	if doctor.Failed(rs) {
		fmt.Fprintln(w, "\nsaddle doctor found failing checks. Fix them, then run this command again.")
		return false
	}
	fmt.Fprintln(w)
	if err := writeMarker(root, "ok"); err != nil {
		fmt.Fprintln(w, "saddle:", err)
		return false
	}
	return true
}

// writePluginTrust is the trust prompt for the plugin: Claude Code captures
// the output, so the session asks the user in chat and records a yes with
// saddle trust --yes (#215). Nothing has been written yet.
func writePluginTrust(w io.Writer, root string, err error) {
	if !errors.Is(err, trust.ErrUntrusted) {
		fmt.Fprintln(w, "saddle could not check whether this repo is trusted:", err)
		return
	}
	fmt.Fprint(w, trust.Prompt(root))
	fmt.Fprintf(w, `
Saddle hasn't written anything here yet. Ask the user the question above, with
both options, and wait for their answer in chat. Don't answer it for them.
- If they choose 1, run `+"`saddle trust --yes`"+` in %s, then run this command again.
- If they choose 2, stop: saddle won't run here.
`, root)
}

func initRepo(root string) error {
	a, err := app.Open(root)
	if err != nil {
		return err
	}
	defer a.Close()
	return a.Init()
}

func writeMarker(root, state string) error {
	return os.WriteFile(filepath.Join(root, ".saddle", onboardMarker), []byte(state+"\n"), 0o644)
}

func systemDoctor(root string) []doctor.Result { return doctor.Run(doctor.System(root)) }

// stdoutIsTTY decides whether the howdy banner prints; tests replace it.
var stdoutIsTTY = banner.IsTerminal

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

// pluginSetupCmd is the plugin commands' first step: it sets saddle up in a
// repo that never ran saddle init (see onboard).
func pluginSetupCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "setup",
		Short: "Set saddle up in this repo on first use: saddle init, then saddle doctor",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if !onboard(out, wd(), systemDoctor) {
				return nil
			}
			if a, why := openPlugin(wd()); a == nil {
				fmt.Fprintln(out, why)
			} else {
				a.Close()
				fmt.Fprintln(out, "Saddle is set up in "+a.Root+".")
			}
			return nil
		},
	}
}

func pluginBriefCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "brief",
		Short: "Print the orchestrator brief and where things stand",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !onboard(cmd.OutOrStdout(), wd(), systemDoctor) {
				return nil
			}
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
			if err := trust.Gate(a.Root, trust.Options{}); err != nil {
				return fmt.Errorf("the saddle engine won't run in a repo you haven't trusted; run `saddle trust` in %s first: %w", a.Root, err)
			}
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

// saddlePkg is what go install builds; pinned to the plugin's version.
const saddlePkg = "github.com/brandonapol/saddle/cmd/saddle"

func init() {
	// go install pkg@vX.Y.Z builds without the Makefile's -ldflags, so the
	// module version is the only place the release version is recorded. The
	// plugin's version check needs it.
	if Version != "dev" {
		return
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		Version = bi.Main.Version
	}
}

type installState int

const (
	installCurrent installState = iota
	installMissing
	installOutdated
	installUnknown // a dev or commit build: no release version to compare
)

// installDeps is how install checks reach PATH and run programs; tests fake it.
type installDeps struct {
	lookPath func(name string) (string, error)
	// run runs a program and returns its trimmed combined output.
	run func(name string, args ...string) (string, error)
}

func systemInstallDeps() installDeps {
	return installDeps{
		lookPath: exec.LookPath,
		run: func(name string, args ...string) (string, error) {
			b, err := exec.Command(name, args...).CombinedOutput()
			return strings.TrimSpace(string(b)), err
		},
	}
}

type installCheck struct {
	State installState
	Have  string
}

// checkInstall compares the saddle on PATH with the version the plugin
// expects. A binary at or past want is current.
func checkInstall(d installDeps, want string) installCheck {
	if _, err := d.lookPath("saddle"); err != nil {
		return installCheck{State: installMissing}
	}
	have, err := d.run("saddle", "version")
	if err != nil {
		return installCheck{State: installMissing, Have: have}
	}
	h1, h2, h3, okH := semver(have)
	w1, w2, w3, okW := semver(want)
	if !okH || !okW {
		return installCheck{State: installUnknown, Have: have}
	}
	for _, p := range [][2]int{{h1, w1}, {h2, w2}, {h3, w3}} {
		if p[0] != p[1] {
			if p[0] < p[1] {
				return installCheck{State: installOutdated, Have: have}
			}
			break
		}
	}
	return installCheck{State: installCurrent, Have: have}
}

// semver parses the X.Y.Z at the start of v ("v0.1.0-3-gabc" is 0.1.0). Go
// pseudo-versions of untagged builds (v0.0.0-...) don't count.
func semver(v string) (major, minor, patch int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var n [3]int
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return 0, 0, 0, false
		}
		n[i] = x
	}
	if n == [3]int{} {
		return 0, 0, 0, false
	}
	return n[0], n[1], n[2], true
}

// runInstall reports whether the saddle on PATH is good for a plugin at
// version want, printing nothing when it is. Otherwise it prints exactly
// what it would run to fix that, and runs it only when yes. Without Go it
// prints instructions and runs nothing.
func runInstall(w io.Writer, d installDeps, want string, yes bool) bool {
	c := checkInstall(d, want)
	switch c.State {
	case installCurrent, installUnknown:
		return true
	case installMissing:
		fmt.Fprintf(w, "The saddle plugin %s needs the saddle binary, and it is not on your PATH.\n", want)
	case installOutdated:
		fmt.Fprintf(w, "saddle %s on your PATH is older than the saddle plugin (%s).\n", c.Have, want)
	}
	cmd := []string{"go", "install", saddlePkg + "@v" + want}
	line := strings.Join(cmd, " ")
	if _, err := d.lookPath("go"); err != nil {
		fmt.Fprintf(w, "Install Go (https://go.dev/dl/), then run:\n  %s\nand put $(go env GOPATH)/bin on your PATH. See docs/QUICKSTART.md.\n", line)
		return false
	}
	if !yes {
		fmt.Fprintf(w, "To install it, saddle will run:\n  %s\nRun `saddle plugin install --yes` to do that now (it changes nothing else).\n", line)
		return false
	}
	fmt.Fprintln(w, "+ "+line)
	if out, err := d.run(cmd[0], cmd[1:]...); err != nil {
		fmt.Fprintf(w, "%s\ngo install failed: %v\n", out, err)
		return false
	}
	if c := checkInstall(d, want); c.State == installMissing || c.State == installOutdated {
		fmt.Fprintf(w, "Installed, but the saddle on your PATH is still %q. Put $(go env GOPATH)/bin first on your PATH.\n", c.Have)
		return false
	}
	fmt.Fprintf(w, "saddle v%s installed.\n", want)
	return true
}

// pluginVersion reads the version from the plugin's manifest under root.
func pluginVersion(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, ".claude-plugin", "plugin.json"))
	if err != nil {
		return "", err
	}
	var m struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return "", fmt.Errorf("plugin.json: %w", err)
	}
	return m.Version, nil
}

func pluginInstallCmd() *cobra.Command {
	var want string
	var yes bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Check the saddle binary against the plugin's version and print how to install it",
		Long: `Compares the saddle on PATH with the version the saddle Claude Code plugin
expects (--want, or the plugin.json under $CLAUDE_PLUGIN_ROOT). When it is
missing or older, prints the exact command that installs the plugin's version
(go install ` + saddlePkg + `@v<version>), and runs it only with --yes.
Prints nothing when the binary is current or a dev build.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if want == "" {
				root := os.Getenv("CLAUDE_PLUGIN_ROOT")
				if root == "" {
					return errors.New("no version to check against: pass --want or set CLAUDE_PLUGIN_ROOT")
				}
				v, err := pluginVersion(root)
				if err != nil {
					return err
				}
				want = v
			}
			runInstall(cmd.OutOrStdout(), systemInstallDeps(), want, yes)
			return nil
		},
	}
	cmd.Flags().StringVar(&want, "want", "", "the plugin version saddle should be at least")
	cmd.Flags().BoolVar(&yes, "yes", false, "run the printed install command")
	return cmd
}
