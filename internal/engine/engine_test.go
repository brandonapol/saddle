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
