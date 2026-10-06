package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/e2e/fakegh"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// Options shape a World.
type Options struct {
	// NoInit skips `saddle init` and the harness config.
	NoInit bool
	// Top is TOML for top-level config keys (concurrency, close_on_land, …).
	Top string
	// Tables is TOML tables appended to the config ([train], [spawn], …).
	Tables string
	// TestCmd is [test] cmd; empty means "test ! -e FAIL_TESTS", so an agent
	// fails the train's tests by committing a FAIL_TESTS file.
	TestCmd string
}

// World is one hermetic saddle sandbox.
type World struct {
	T       testing.TB
	Bins    Bins
	Root    string // the temp dir everything lives in
	Home    string
	Origin  string // the bare origin, which is also the fake GitHub
	Repo    string // the main checkout
	Scripts string // fake agent scripts and logs
	Bin     string // this world's bin dir: the claude wrapper
	Tmux    *Tmux
	GH      fakegh.Repo
	env     []string
}

// New builds a World: a temp HOME with a git identity, a bare origin on
// main with a fake GitHub, a clone with one commit, a private tmux server
// and, unless opts.NoInit, `saddle init` with the harness config. Worlds
// set process env (HOME, TMUX_TMPDIR) for in-process saddle calls, so tests
// using them can't run in parallel.
func New(t testing.TB, bins Bins, opts Options) *World {
	t.Helper()
	root := t.TempDir()
	w := &World{T: t, Bins: bins, Root: root, Home: filepath.Join(root, "home"), Origin: filepath.Join(root, "origin.git"),
		Repo: filepath.Join(root, "demo"), Scripts: filepath.Join(root, "scripts"), Bin: filepath.Join(root, "bin")}
	for _, d := range []string{w.Home, w.Scripts, w.Bin, filepath.Join(w.Home, ".config")} {
		must(t, os.MkdirAll(d, 0o755))
	}
	must(t, os.WriteFile(filepath.Join(w.Home, ".gitconfig"), []byte(
		"[user]\n\tname = E2E\n\temail = e2e@example.com\n[init]\n\tdefaultBranch = main\n[commit]\n\tgpgsign = false\n[advice]\n\tdetachedHead = false\n"), 0o644))
	wrapper := fmt.Sprintf("#!/bin/sh\nexec %s --script-dir %s \"$@\"\n", shq(bins.Agent), shq(w.Scripts))
	must(t, os.WriteFile(filepath.Join(w.Bin, "claude"), []byte(wrapper), 0o755))

	w.Tmux = NewTmux(t)
	// Runs before NewTmux's kill-server and before t.TempDir is removed.
	t.Cleanup(func() { stopTmux(w.Tmux) })
	w.env = []string{
		"HOME=" + w.Home,
		"XDG_CONFIG_HOME=" + filepath.Join(w.Home, ".config"),
		"PATH=" + w.Bin + string(os.PathListSeparator) + bins.Dir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"TMUX_TMPDIR=" + w.Tmux.Dir,
		"TMPDIR=" + os.TempDir(),
		"TERM=xterm-256color",
		"LANG=C.UTF-8",
		"SHELL=/bin/sh",
		"USER=e2e",
	}
	w.Tmux.Env = w.env
	// In-process saddle calls (App from Open) see the same HOME and tmux.
	for _, kv := range w.env {
		k, v, _ := strings.Cut(kv, "=")
		if k != "PATH" {
			t.Setenv(k, v)
		}
	}
	for _, k := range []string{"TMUX", "SADDLE_ROOT", "SADDLE_TASK"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}

	w.Git(root, "init", "-q", "--bare", "-b", "main", w.Origin)
	var err error
	w.GH, err = fakegh.Init(w.Origin, "e2e", "demo")
	must(t, err)
	must(t, os.MkdirAll(w.Repo, 0o755))
	w.Git(w.Repo, "init", "-q", "-b", "main")
	w.WriteFile("README.md", "# demo\n")
	w.Git(w.Repo, "add", "-A")
	w.Git(w.Repo, "commit", "-q", "-m", "initial commit")
	w.Git(w.Repo, "remote", "add", "origin", w.Origin)
	w.Git(w.Repo, "push", "-q", "-u", "origin", "main")
	w.Git(w.Repo, "remote", "set-head", "origin", "main")
	if opts.NoInit {
		return w
	}
	w.MustSaddle("init", "-q", "--trust")
	w.WriteConfig(opts)
	return w
}

// WriteConfig replaces .saddle/config.toml with the harness config: the
// fake agent as claude, opts' test command and extra TOML.
func (w *World) WriteConfig(opts Options) {
	test := opts.TestCmd
	if test == "" {
		test = "test ! -e FAIL_TESTS"
	}
	cfg := fmt.Sprintf("%s\n[test]\ncmd = %q\n\n[claude]\ncmd = %q\n\n[triage]\ndisabled = true\n\n%s\n",
		opts.Top, test, filepath.Join(w.Bin, "claude"), opts.Tables)
	must(w.T, os.WriteFile(filepath.Join(w.Repo, ".saddle", "config.toml"), []byte(cfg), 0o644))
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Env is the environment saddle runs with in this World.
func (w *World) Env() []string { return append([]string(nil), w.env...) }

// Result is one finished command.
type Result struct {
	Args   []string
	Stdout string
	Stderr string
	Code   int
}

func (r Result) String() string {
	return fmt.Sprintf("%s (exit %d)\nstdout:\n%s\nstderr:\n%s", strings.Join(r.Args, " "), r.Code, r.Stdout, r.Stderr)
}

// Exec runs name in dir with the World's env.
func (w *World) Exec(dir, name string, args ...string) Result {
	w.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*Timeout())
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env = dir, w.env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	r := Result{Args: append([]string{filepath.Base(name)}, args...), Stdout: out.String(), Stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		r.Code = ee.ExitCode()
	case err != nil:
		w.T.Fatalf("run %s: %v", strings.Join(r.Args, " "), err)
	}
	return r
}

// Saddle runs the saddle binary in the main checkout.
func (w *World) Saddle(args ...string) Result {
	w.T.Helper()
	return w.Exec(w.Repo, w.Bins.Saddle, args...)
}

// SaddleIn runs the saddle binary in dir.
func (w *World) SaddleIn(dir string, args ...string) Result {
	w.T.Helper()
	return w.Exec(dir, w.Bins.Saddle, args...)
}

// MustSaddle runs saddle and fails the test unless it exits 0.
func (w *World) MustSaddle(args ...string) Result {
	w.T.Helper()
	r := w.Saddle(args...)
	if r.Code != 0 {
		w.T.Fatalf("saddle failed: %s", r)
	}
	return r
}

// Git runs git in dir and returns trimmed stdout, failing the test on error.
func (w *World) Git(dir string, args ...string) string {
	w.T.Helper()
	r := w.Exec(dir, "git", args...)
	if r.Code != 0 {
		w.T.Fatalf("git failed: %s", r)
	}
	return strings.TrimSpace(r.Stdout)
}

// WriteFile writes a file in the main checkout.
func (w *World) WriteFile(rel, body string) {
	w.T.Helper()
	p := filepath.Join(w.Repo, rel)
	must(w.T, os.MkdirAll(filepath.Dir(p), 0o755))
	must(w.T, os.WriteFile(p, []byte(body), 0o644))
}

// Status is `saddle status --json`.
func (w *World) Status() mcpserver.StatusOut {
	w.T.Helper()
	r := w.MustSaddle("status", "--json")
	var st mcpserver.StatusOut
	if err := json.Unmarshal([]byte(r.Stdout), &st); err != nil {
		w.T.Fatalf("status --json: %v\n%s", err, r.Stdout)
	}
	return st
}

// Task is task id from status, or a zero TaskView.
func (w *World) Task(id string) mcpserver.TaskView {
	w.T.Helper()
	for _, t := range w.Status().Tasks {
		if t.ID == id {
			return t
		}
	}
	return mcpserver.TaskView{}
}

// WaitTask waits until task id satisfies ok, describing it as what.
func (w *World) WaitTask(id, what string, ok func(mcpserver.TaskView) bool) mcpserver.TaskView {
	w.T.Helper()
	var last mcpserver.TaskView
	Eventually(w.T, id+" "+what, func() error {
		last = w.Task(id)
		if ok(last) {
			return nil
		}
		return fmt.Errorf("last: %+v", last)
	})
	return last
}

// WaitStatus waits until task id has status.
func (w *World) WaitStatus(id, status string) mcpserver.TaskView {
	w.T.Helper()
	return w.WaitTask(id, "status "+status, func(t mcpserver.TaskView) bool { return t.Status == status })
}

// Spawn scripts a fake agent and spawns it as task id with claims.
func (w *World) Spawn(id, title string, claims []string, steps ...fakeagent.Step) Result {
	w.T.Helper()
	must(w.T, fakeagent.Script{Steps: steps}.Save(w.Scripts, id))
	args := []string{"spawn", "--id", id}
	for _, c := range claims {
		args = append(args, "-c", c)
	}
	return w.MustSaddle(append(args, title, "Prompt for "+title)...)
}

// AgentLog is everything task's fake agent logged.
func (w *World) AgentLog(id string) string {
	b, _ := os.ReadFile(fakeagent.LogPath(w.Scripts, id))
	return string(b)
}

// WaitAgentLog waits until task's fake agent logged a line containing want.
func (w *World) WaitAgentLog(id, want string) {
	w.T.Helper()
	Eventually(w.T, id+"'s agent to log "+want, func() error {
		if strings.Contains(w.AgentLog(id), want) {
			return nil
		}
		return fmt.Errorf("log so far:\n%s", w.AgentLog(id))
	})
}

// GHState is a snapshot of the fake GitHub.
func (w *World) GHState() *fakegh.State {
	w.T.Helper()
	s, err := w.GH.Load()
	must(w.T, err)
	return s
}

// OriginFile is a file's content on a branch of origin, or "" if missing.
func (w *World) OriginFile(branch, rel string) string {
	r := w.Exec(w.Origin, "git", "--git-dir", w.Origin, "show", branch+":"+rel)
	if r.Code != 0 {
		return ""
	}
	return r.Stdout
}

// Diagnose logs what a failed journey needs to be debugged: status, the
// agents' logs and windows, the fake GitHub's calls.
func (w *World) Diagnose() {
	w.T.Helper()
	if !w.T.Failed() {
		return
	}
	if r := w.Saddle("status"); true {
		w.T.Logf("saddle status:\n%s%s", r.Stdout, r.Stderr)
	}
	ents, _ := os.ReadDir(w.Scripts)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".log") {
			b, _ := os.ReadFile(filepath.Join(w.Scripts, e.Name()))
			w.T.Logf("%s:\n%s", e.Name(), b)
		}
	}
	if s, err := w.GH.Load(); err == nil {
		for _, c := range s.Calls {
			w.T.Logf("gh %s", strings.Join(c, " "))
		}
	}
	if out, err := w.Tmux.Run("list-windows", "-a"); err == nil {
		w.T.Logf("tmux windows:\n%s", out)
	}
}

// stopTmux kills x's server and waits (up to 5s) for every pane's session
// to exit. An agent's hook can outlive kill-server by a moment and reopen
// .saddle/state.db, recreating its -wal and -shm while t.TempDir is being
// removed ("directory not empty").
func stopTmux(x *Tmux) {
	pids, _ := x.Run("list-panes", "-a", "-F", "#{pane_pid}")
	_, _ = x.Run("kill-server")
	deadline := time.Now().Add(5 * time.Second)
	for _, pid := range strings.Fields(pids) {
		for time.Now().Before(deadline) && exec.Command("pgrep", "-s", pid).Run() == nil {
			time.Sleep(20 * time.Millisecond)
		}
	}
}
