package tui

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// #321: under saddle up, a worker sitting on a usage-limit banner marks its
// adapter out of quota (the TUI has no engine doing it), once, and sends no
// keys into the pane.
func TestWatchScreensLimitBannerRotatesAdapter(t *testing.T) {
	m := newViewModel(120, 40)
	m.app.Root = t.TempDir()
	st, err := store.Open(filepath.Join(m.app.Root, ".saddle", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m.app.Store = st
	m.app.AdapterStatus = func() []agent.Status {
		return []agent.Status{{Name: "claude", OK: true}, {Name: "grok", OK: true}, {Name: "codex", OK: true}}
	}
	var told []string
	m.app.OwnerNotify = func(s string) { told = append(told, s) }
	if err := st.CreateTask(store.Task{ID: "t1", Title: "view router", Role: store.RoleWorker, Status: store.Running}); err != nil {
		t.Fatal(err)
	}
	screen := "You've hit your session limit · resets 3pm\n> "
	ts := []mcpserver.TaskView{{ID: "t1", Status: store.Running, Window: "@1"}}
	m.screens = map[string]*screenState{"t1": {text: screen, since: time.Now().Add(-time.Minute)}}
	m.watchScreens(ts, map[string]string{"t1": screen})
	m.watchScreens(ts, map[string]string{"t1": screen})
	if _, out := m.app.ExhaustedAdapters(time.Now())[usage.Claude]; !out {
		t.Fatal("claude not marked out of quota")
	}
	if len(told) != 1 || !strings.Contains(told[0], "grok") {
		t.Fatalf("owner told %q", told)
	}
	if s, _ := st.Task("t1"); s.Status == store.NeedsYou {
		t.Error("a usage-limit banner is not a prompt that needs the owner")
	}
}

// #321: the footer shows the rotation banner while an adapter is out.
func TestFooterShowsRotationBanner(t *testing.T) {
	m := newViewModel(120, 40)
	m.rotation = "out of quota: claude until 3pm; new spawns go to grok"
	if v := m.View(); !strings.Contains(v, "claude until 3pm") {
		t.Fatalf("view lacks the rotation banner:\n%s", v)
	}
	m.rotation = ""
	if v := m.View(); strings.Contains(v, "out of quota") {
		t.Fatal("banner shown with nothing out")
	}
}

// typingTmux records what is typed into each window.
type typingTmux struct {
	mu    sync.Mutex
	typed []string
}

func (f *typingTmux) HasSession() bool                          { return true }
func (f *typingTmux) NewSession(_, _, _ string) (string, error) { return "", nil }
func (f *typingTmux) NewWindow(_, _, _ string) (string, error)  { return "", nil }
func (f *typingTmux) KillWindow(string) error                   { return nil }
func (f *typingTmux) Alive(string) bool                         { return true }
func (f *typingTmux) SendKeys(string, ...string) error          { return nil }
func (f *typingTmux) KillSession() error                        { return nil }
func (f *typingTmux) Capture(string, int) (string, error)       { return "", nil }
func (f *typingTmux) SendText(_, s string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.typed = append(f.typed, s)
	return nil
}

func (f *typingTmux) all() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.typed...)
}

// #480: under saddle up, a codex worker on its usage-limit error is typed a
// continue line once codex has quota again, and only once.
func TestWatchScreensResumesCodexAfterReset(t *testing.T) {
	m := newViewModel(120, 40)
	m.app.Root = t.TempDir()
	st, err := store.Open(filepath.Join(m.app.Root, ".saddle", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m.app.Store = st
	tm := &typingTmux{}
	m.app.Tmux = tm
	m.app.AdapterStatus = func() []agent.Status {
		return []agent.Status{{Name: "claude", OK: true}, {Name: "grok", OK: true}, {Name: "codex", OK: true}}
	}
	m.app.OwnerNotify = func(string) {}
	if err := st.CreateTask(store.Task{ID: "t1", Title: "view router", Role: store.RoleWorker, Status: store.Running, Window: "@1"}); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(m.app.Root, ".saddle", "run", "t1")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "adapter"), []byte("codex\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	screen := "■ You've hit your usage limit. Upgrade to Pro or try again at 3pm.\n› "
	ts := []mcpserver.TaskView{{ID: "t1", Status: store.Running, Window: "@1"}}
	m.screens = map[string]*screenState{"t1": {text: screen, since: time.Now().Add(-time.Minute)}}
	m.watchScreens(ts, map[string]string{"t1": screen}) // marks codex out until 3pm
	m.watchScreens(ts, map[string]string{"t1": screen})
	time.Sleep(50 * time.Millisecond)
	if got := tm.all(); len(got) != 0 {
		t.Fatalf("typed %q while codex is out of quota", got)
	}
	if err := m.app.ClearExhausted(usage.Codex); err != nil { // the reset passed
		t.Fatal(err)
	}
	for range 3 {
		m.watchScreens(ts, map[string]string{"t1": screen})
	}
	deadline := time.Now().Add(3 * time.Second)
	for len(tm.all()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)
	if got := tm.all(); len(got) != 1 || got[0] != app.LimitResumeLine {
		t.Fatalf("typed %q after the reset, want the continue line once", got)
	}
}

// #480: under saddle up a codex worker sitting still at its composer is
// marked idle (it has no Stop hook), and working again marks it running.
func TestWatchScreensMarksCodexIdle(t *testing.T) {
	m := newViewModel(120, 40)
	m.app.Root = t.TempDir()
	st, err := store.Open(filepath.Join(m.app.Root, ".saddle", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m.app.Store = st
	m.app.Tmux = &typingTmux{}
	if err := st.CreateTask(store.Task{ID: "t1", Title: "view router", Role: store.RoleWorker, Status: store.Running, Window: "@1"}); err != nil {
		t.Fatal(err)
	}
	run := filepath.Join(m.app.Root, ".saddle", "run", "t1")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "adapter"), []byte("codex\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	screen := "• Done.\n\n› Ask Codex to do anything"
	m.screens = map[string]*screenState{"t1": {text: screen, since: time.Now().Add(-time.Minute)}}
	m.watchScreens([]mcpserver.TaskView{{ID: "t1", Status: store.Running, Window: "@1"}}, map[string]string{"t1": screen})
	if s, _ := st.Task("t1"); s.Status != store.Idle {
		t.Fatalf("codex at rest is %q, want idle", s.Status)
	}
	m.watchScreens([]mcpserver.TaskView{{ID: "t1", Status: store.Idle, Window: "@1"}}, map[string]string{"t1": "• Working (2s • esc to interrupt)"})
	if s, _ := st.Task("t1"); s.Status != store.Running {
		t.Fatalf("codex working is %q, want running", s.Status)
	}
}
