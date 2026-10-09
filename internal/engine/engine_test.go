package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// screenTmux shows each window the screen set for it.
type screenTmux struct {
	n       int
	screens map[string]string
	keys    map[string][]string
}

func (f *screenTmux) HasSession() bool                          { return true }
func (f *screenTmux) NewSession(n, d, c string) (string, error) { return f.NewWindow(n, d, c) }
func (f *screenTmux) NewWindow(string, string, string) (string, error) {
	f.n++
	return fmt.Sprintf("@%d", f.n), nil
}
func (f *screenTmux) KillWindow(string) error       { return nil }
func (f *screenTmux) Alive(string) bool             { return true }
func (f *screenTmux) SendText(string, string) error { return nil }
func (f *screenTmux) SendKeys(w string, k ...string) error {
	f.keys[w] = append(f.keys[w], k...)
	return nil
}
func (f *screenTmux) Capture(w string, _ int) (string, error) { return f.screens[w], nil }
func (f *screenTmux) KillSession() error                      { return nil }

func setup(t *testing.T) (*app.App, *screenTmux) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir()) // RootTrusted reads ~/.claude.json
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
	ft := &screenTmux{screens: map[string]string{}, keys: map[string][]string{}}
	a.Tmux = ft
	if _, err := a.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	return a, ft
}

func orchNotices(t *testing.T, a *app.App) string {
	t.Helper()
	ns, err := a.Store.TakeNotices(app.OrchestratorID, true)
	if err != nil {
		t.Fatal(err)
	}
	return store.FormatNotices(ns)
}

func TestEngineReportsWorkerThatStoppedWithoutDone(t *testing.T) {
	a, ft := setup(t)
	w, err := a.Spawn(app.SpawnReq{Title: "meter"})
	if err != nil {
		t.Fatal(err)
	}
	ft.screens[w.Window] = "working…"
	e := New(a)
	must(t, e.Tick())
	orchNotices(t, a) // spawn notices, if any
	must(t, a.Store.SetStatus(w.ID, store.Idle))
	ft.screens[w.Window] = "all done, I think"
	must(t, e.Tick())
	got := orchNotices(t, a)
	if !strings.Contains(got, w.ID+" (meter) stopped without calling done") || !strings.Contains(got, "all done, I think") {
		t.Fatalf("notices %q", got)
	}
	must(t, e.Tick())
	if got := orchNotices(t, a); got != "" {
		t.Fatalf("reported twice: %q", got)
	}
}

func TestEngineFirstTickDoesNotReplayOldStates(t *testing.T) {
	a, _ := setup(t)
	w, err := a.Spawn(app.SpawnReq{Title: "meter"})
	if err != nil {
		t.Fatal(err)
	}
	must(t, a.Store.SetStatus(w.ID, store.Idle))
	orchNotices(t, a)
	must(t, New(a).Tick())
	if got := orchNotices(t, a); got != "" {
		t.Fatalf("a restarted engine re-reported: %q", got)
	}
}

func TestEngineReportsPromptThatSits(t *testing.T) {
	a, ft := setup(t)
	w, err := a.Spawn(app.SpawnReq{Title: "meter"})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	e := New(a)
	e.now = func() time.Time { return clock }
	must(t, e.Tick())
	orchNotices(t, a)

	ft.screens[w.Window] = "Do you want to proceed?\n❯ 1. Yes\n  2. No"
	must(t, e.Tick()) // first sight of the screen
	if got := orchNotices(t, a); got != "" {
		t.Fatalf("reported before the prompt settled: %q", got)
	}
	clock = clock.Add(5 * time.Second)
	must(t, e.Tick())
	if st, _ := a.Store.Task(w.ID); st.Status != store.NeedsYou {
		t.Fatalf("status %q", st.Status)
	}
	got := orchNotices(t, a)
	if !strings.Contains(got, w.ID+" (meter) is waiting on a permission prompt") || !strings.Contains(got, "Do you want to proceed?") {
		t.Fatalf("notices %q", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// limitScreen is Claude Code parked on a session usage limit (#180).
const limitScreen = `You've hit your session limit · resets 12:10pm (America/New_York)

● Usage limit reached · continuing automatically at 12:10pm · esc to cancel
╭────────────╮
│ >          │
╰────────────╯
  ⚠ Usage limit reached · limit resets 12:10pm`

func TestEngineParksWorkerOnUsageLimit(t *testing.T) {
	a, ft := setup(t)
	w, err := a.Spawn(app.SpawnReq{Title: "meter"})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	e := New(a)
	e.now = func() time.Time { return clock }
	ft.screens[w.Window] = "working…"
	must(t, e.Tick())
	orchNotices(t, a)

	ft.screens[w.Window] = limitScreen
	must(t, e.Tick())
	clock = clock.Add(5 * time.Second)
	must(t, e.Tick())
	if st, _ := a.Store.Task(w.ID); st.Status != app.StatusPaused {
		t.Fatalf("status %q while the banner is up", st.Status)
	}
	got := orchNotices(t, a)
	if !strings.Contains(got, w.ID+" (meter) is parked on a Claude usage limit") || !strings.Contains(got, "12:10pm (America/New_York)") {
		t.Fatalf("notices %q", got)
	}
	for range 3 {
		clock = clock.Add(5 * time.Second)
		must(t, e.Tick())
	}
	if got := orchNotices(t, a); got != "" {
		t.Fatalf("nagged again: %q", got)
	}
	if k := ft.keys[w.Window]; len(k) > 0 {
		t.Fatalf("sent keys into a parked pane: %v", k)
	}

	ft.screens[w.Window] = "⏺ Picking up where I left off"
	must(t, e.Tick())
	if st, _ := a.Store.Task(w.ID); st.Status != store.Running {
		t.Fatalf("status %q after the banner cleared", st.Status)
	}
	clock = clock.Add(5 * time.Second)
	must(t, e.Tick())
	if got := orchNotices(t, a); got != "" {
		t.Fatalf("notified on resume: %q", got)
	}
}

// Claude Code's Stop hook marks the worker idle as it hits the limit; that
// is the park, not a worker that stopped without calling done.
func TestEngineUsageLimitIsNotStoppedWithoutDone(t *testing.T) {
	a, ft := setup(t)
	w, err := a.Spawn(app.SpawnReq{Title: "meter"})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	e := New(a)
	e.now = func() time.Time { return clock }
	ft.screens[w.Window] = "working…"
	must(t, e.Tick())
	orchNotices(t, a)

	must(t, a.Store.SetStatus(w.ID, store.Idle))
	ft.screens[w.Window] = limitScreen
	must(t, e.Tick())
	if got := orchNotices(t, a); got != "" {
		t.Fatalf("reported the park as a stop: %q", got)
	}
	clock = clock.Add(5 * time.Second)
	must(t, e.Tick())
	got := orchNotices(t, a)
	if strings.Contains(got, "stopped without calling done") || !strings.Contains(got, "parked on a Claude usage limit") {
		t.Fatalf("notices %q", got)
	}
	// The hook fires again while the banner is up: back to paused, quietly.
	must(t, a.Store.SetStatus(w.ID, store.Idle))
	clock = clock.Add(5 * time.Second)
	must(t, e.Tick())
	if st, _ := a.Store.Task(w.ID); st.Status != app.StatusPaused {
		t.Fatalf("status %q", st.Status)
	}
	if got := orchNotices(t, a); got != "" {
		t.Fatalf("nagged again: %q", got)
	}
}

// A restarted engine finds the worker it parked still paused on the banner,
// keeps quiet about it and still puts it back to running after the reset.
func TestRestartedEngineResumesUsageParkedWorker(t *testing.T) {
	a, ft := setup(t)
	w, err := a.Spawn(app.SpawnReq{Title: "meter"})
	if err != nil {
		t.Fatal(err)
	}
	must(t, a.Store.SetStatus(w.ID, app.StatusPaused))
	ft.screens[w.Window] = limitScreen
	orchNotices(t, a)
	clock := time.Now()
	e := New(a)
	e.now = func() time.Time { return clock }
	must(t, e.Tick())
	clock = clock.Add(5 * time.Second)
	must(t, e.Tick())
	if got := orchNotices(t, a); got != "" {
		t.Fatalf("re-notified after a restart: %q", got)
	}
	ft.screens[w.Window] = "⏺ Back at it"
	must(t, e.Tick())
	if st, _ := a.Store.Task(w.ID); st.Status != store.Running {
		t.Fatalf("status %q after the banner cleared", st.Status)
	}
}

// A task saddle down paused, whose window is gone, is not the engine's to
// resume.
func TestEngineLeavesDownPausedTaskAlone(t *testing.T) {
	a, ft := setup(t)
	w, err := a.Spawn(app.SpawnReq{Title: "meter"})
	if err != nil {
		t.Fatal(err)
	}
	must(t, a.Store.SetStatus(w.ID, app.StatusPaused))
	ft.screens[w.Window] = "⏺ last words"
	e := New(a)
	for range 3 {
		must(t, e.Tick())
	}
	if st, _ := a.Store.Task(w.ID); st.Status != app.StatusPaused {
		t.Fatalf("status %q", st.Status)
	}
}
