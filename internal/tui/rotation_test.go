package tui

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
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
