package orchrc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// recTmux records every window it opens and anything typed into one.
type recTmux struct {
	mu      sync.Mutex
	n       int
	windows map[string]bool
	cmds    map[string]string
	typed   []string
}

func (f *recTmux) HasSession() bool                          { return true }
func (f *recTmux) NewSession(n, d, c string) (string, error) { return f.NewWindow(n, d, c) }
func (f *recTmux) NewWindow(_, _, cmd string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	id := fmt.Sprintf("@%d", f.n)
	f.windows[id], f.cmds[id] = true, cmd
	return id, nil
}
func (f *recTmux) KillWindow(id string) error { f.mu.Lock(); delete(f.windows, id); f.mu.Unlock(); return nil }
func (f *recTmux) Alive(id string) bool       { f.mu.Lock(); defer f.mu.Unlock(); return f.windows[id] }
func (f *recTmux) SendText(id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.typed = append(f.typed, id+": "+text)
	return nil
}
func (f *recTmux) SendKeys(id string, keys ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.typed = append(f.typed, id+": keys "+strings.Join(keys, " "))
	return nil
}
func (f *recTmux) Capture(string, int) (string, error) { return "> ", nil }
func (f *recTmux) KillSession() error                  { return nil }

func setup(t *testing.T) (*app.App, *recTmux) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("SADDLE_ROOT", "")
	t.Setenv("SADDLE_TASK", "")
	t.Setenv("SADDLE_TRAIN", "")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-qm", "init"}} {
		if _, err := gitx.Run(root, args...); err != nil {
			t.Fatal(err)
		}
	}
	a, err := app.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	ft := &recTmux{windows: map[string]bool{}, cmds: map[string]string{}}
	a.Tmux = ft
	return a, ft
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// launchScript reads the script a window command (bash '<path>') runs.
func launchScript(t *testing.T, cmd string) string {
	t.Helper()
	i := strings.Index(cmd, "bash '")
	if i < 0 {
		t.Fatalf("window command runs no launch script: %s", cmd)
	}
	path := cmd[i+len("bash '"):]
	path = path[:strings.IndexByte(path, '\'')]
	return read(t, path)
}

// --rc builds an interactive launch: no -p or stream-json, with
// --remote-control and the repo's name, and the same settings, MCP config,
// model and orchestrator brief as the headless orchestrator.
func TestRCLaunchIsInteractiveWithTheHeadlessConfig(t *testing.T) {
	a, _ := setup(t)
	hl, _, err := a.Orchestrator()
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := hl.Headless("")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cmd.Args, "-p") {
		t.Fatalf("the headless launch isn't headless: %v", cmd.Args)
	}
	settings := read(t, filepath.Join(hl.RunDir, "settings.json"))
	mcp := read(t, filepath.Join(hl.RunDir, "mcp.json"))

	var st State
	l, err := Launch(a, &st, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if l.RunDir != hl.RunDir || l.Model != hl.Model || l.Dir != hl.Dir {
		t.Errorf("rc launch run dir %q model %q dir %q, headless %q %q %q", l.RunDir, l.Model, l.Dir, hl.RunDir, hl.Model, hl.Dir)
	}
	sh, err := l.Write()
	if err != nil {
		t.Fatal(err)
	}
	script := launchScript(t, sh)
	for _, bad := range []string{" -p ", "stream-json", "--print"} {
		if strings.Contains(script, bad) {
			t.Errorf("rc launch has %q:\n%s", bad, script)
		}
	}
	for _, want := range []string{
		"--remote-control " + shellQuote("saddle: "+filepath.Base(a.Root)),
		`--settings "$run/settings.json"`, `--mcp-config "$run/mcp.json"`,
		"--session-id " + shellQuote(st.SessionID),
		"--disallowedTools",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("rc launch lacks %q:\n%s", want, script)
		}
	}
	if hl.Model != "" && !strings.Contains(script, "--model "+shellQuote(hl.Model)) {
		t.Errorf("rc launch doesn't run the orchestrator model %q:\n%s", hl.Model, script)
	}
	if got := read(t, filepath.Join(l.RunDir, "settings.json")); got != settings {
		t.Errorf("settings differ from headless:\n%s\nvs\n%s", got, settings)
	}
	if got := read(t, filepath.Join(l.RunDir, "mcp.json")); got != mcp {
		t.Errorf("MCP config differs from headless:\n%s\nvs\n%s", got, mcp)
	}
	brief := read(t, filepath.Join(l.RunDir, "brief.md"))
	for _, want := range []string{"## Taking work", "## Watching", "## Getting unstuck", "## Remote Control", "saddle plugin wait", "PushNotification", "saddle remote pause"} {
		if !strings.Contains(brief, want) {
			t.Errorf("rc brief lacks %q", want)
		}
	}
}

// An interrupt notice for an idle Remote Control orchestrator waits for the
// plugin's wake path (saddle plugin wait / the mod take pending action
// notices while saddle up --rc holds the lock). Nothing ever types into its
// window, not even the idle waker.
func TestInterruptWakesIdleRCOrchestratorWithoutSendKeys(t *testing.T) {
	a, ft := setup(t)
	s, err := Start(a, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !ft.Alive(s.Window) {
		t.Fatal("no orchestrator window opened")
	}
	release, err := a.AcquireLock(app.LockUpRC)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := a.Store.SetStatus(app.OrchestratorID, store.Idle); err != nil {
		t.Fatal(err)
	}
	if err := a.Notify(app.OrchestratorID, store.NoticeAction, "t3 (db) needs you: Postgres or SQLite?"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.WakeIdle(time.Now().Add(24 * time.Hour)); err != nil {
		t.Fatal(err)
	}

	if !app.SessionWakes(a.LockOwner()) {
		t.Errorf("lock owner %q leaves t0's notices to the TUI, not the session's wake path", a.LockOwner())
	}
	if n, err := a.Store.PendingActionNotices(app.OrchestratorID); err != nil || n != 1 {
		t.Errorf("pending action notices = %d (%v), want 1 for the wake path to take", n, err)
	}
	ft.mu.Lock()
	typed := slices.Clone(ft.typed)
	ft.mu.Unlock()
	if len(typed) > 0 {
		t.Errorf("saddle typed into the orchestrator's pane: %q", typed)
	}
	t0, err := a.Store.Task(app.OrchestratorID)
	if err != nil {
		t.Fatal(err)
	}
	if t0.Window != "" {
		t.Errorf("t0 records window %q, which would let Notify type into it", t0.Window)
	}
}

// The session id is chosen once, recorded in .saddle/, and every later
// saddle up --rc resumes it.
func TestSessionIDRecordedAndReused(t *testing.T) {
	a, ft := setup(t)
	s, err := Start(a, "plan the epic", false)
	if err != nil {
		t.Fatal(err)
	}
	id := s.SessionID
	if id == "" || s.Resumed {
		t.Fatalf("first start: %+v", s)
	}
	st, err := Load(a.Root)
	if err != nil || st.SessionID != id || st.Window != s.Window {
		t.Fatalf("recorded %+v (%v), want session %s window %s", st, err, id, s.Window)
	}
	first := launchScript(t, ft.cmds[s.Window])
	if !strings.Contains(first, "--session-id "+shellQuote(id)) || strings.Contains(first, "--resume") {
		t.Errorf("first launch doesn't start session %s:\n%s", id, first)
	}

	// While the window lives, a restart reattaches to it.
	again, err := Start(a, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !again.Reused || again.Window != s.Window || again.SessionID != id || ft.n != 1 {
		t.Errorf("restart with the window alive: %+v, %d windows opened", again, ft.n)
	}

	// After the window is gone, the conversation Claude Code kept is resumed.
	_ = ft.KillWindow(s.Window)
	tr := TranscriptPath(a.Root, id)
	if err := os.MkdirAll(filepath.Dir(tr), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tr, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s2, err := Start(a, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if s2.SessionID != id || !s2.Resumed || s2.Window == s.Window {
		t.Fatalf("restart: %+v, want session %s resumed in a new window", s2, id)
	}
	second := launchScript(t, ft.cmds[s2.Window])
	if !strings.Contains(second, "--resume "+shellQuote(id)) || strings.Contains(second, "--session-id") {
		t.Errorf("restart doesn't resume session %s:\n%s", id, second)
	}
	if !strings.Contains(second, "--remote-control") {
		t.Errorf("the resumed session isn't under Remote Control:\n%s", second)
	}
}

func TestPushRuleFollowsRemotePush(t *testing.T) {
	if !strings.Contains(rcSection(false), "call the PushNotification tool") {
		t.Error("without [remote.push] the session isn't told to push")
	}
	if !strings.Contains(rcSection(true), "Don't call PushNotification for those") {
		t.Error("with [remote.push] the session would push interrupts twice")
	}
}
