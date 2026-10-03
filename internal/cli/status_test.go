package cli

import (
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// #37: the tmux segment counts running, needs-you and queued workers and
// shows auto-merge, from the store alone.
func TestTmuxSegmentCounts(t *testing.T) {
	a := storeApp(t)
	addTask(t, a, store.Task{ID: "t0", Title: "orchestrator", Status: store.Running, Role: store.RoleOrchestrator})
	for id, st := range map[string]string{"t1": store.Running, "t2": store.Idle, "t3": store.NeedsYou, "t4": store.Done, "t5": store.Done, "t6": store.Landed} {
		addTask(t, a, store.Task{ID: id, Title: id, Status: st})
	}
	for _, id := range []string{"t4", "t5"} {
		if err := a.Store.Enqueue(id); err != nil {
			t.Fatal(err)
		}
	}
	seg, err := loadSegment(a, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if seg.Running != 2 || seg.NeedsYou != 1 || seg.Queued != 2 {
		t.Errorf("counts: %+v", seg)
	}
	if got := seg.render(false, 0); got != "2 run · 1 you · 2 queued · am off" {
		t.Errorf("segment: %q", got)
	}
}

func TestTmuxSegmentFormat(t *testing.T) {
	s := segment{Running: 12, NeedsYou: 3, Queued: 4, AutoMerge: "on", Usage: usage.Warn, UsageNote: "5h 85%"}
	plain := s.render(false, 0)
	if plain != "12 run · 3 you · 4 queued · am on · ! 5h 85%" {
		t.Errorf("plain: %q", plain)
	}
	if strings.Contains(plain, "#[") || strings.Contains(plain, "\x1b") || strings.Contains(plain, "\n") {
		t.Errorf("plain output must have no color codes or newline: %q", plain)
	}
	if n := utf8.RuneCountInString(plain); n > 48 {
		t.Errorf("segment is %d wide: %q", n, plain)
	}
	over := segment{Usage: usage.Over, UsageNote: "weekly 104%", AutoMerge: "stopped"}.render(false, 0)
	if over != "0 run · 0 you · 0 queued · am stopped · !! weekly 104%" {
		t.Errorf("over: %q", over)
	}
	// A width cap drops the least useful parts first, then clips.
	for _, w := range []int{10, 16, 24, 30} {
		got := s.render(false, w)
		if n := utf8.RuneCountInString(got); n > w {
			t.Errorf("width %d: %q is %d wide", w, got, n)
		}
		if !strings.Contains(got, "3 you") && w >= 10 {
			t.Errorf("width %d dropped needs-you: %q", w, got)
		}
	}
	if got := s.render(false, 25); got != "12 run · 3 you · ! 5h 85%" {
		t.Errorf("width 25: %q", got)
	}
	// --color uses tmux style codes, never ANSI, and only where they matter.
	col := s.render(true, 0)
	if !strings.Contains(col, "#[fg=red]3 you#[default]") || !strings.Contains(col, "#[fg=yellow]! 5h 85%#[default]") || strings.Contains(col, "\x1b") {
		t.Errorf("color: %q", col)
	}
	if got := (segment{}).render(true, 0); strings.Contains(got, "#[") {
		t.Errorf("nothing to flag should stay plain: %q", got)
	}
}

func TestStatusTmuxFlag(t *testing.T) {
	c := withTmux(statusCmd())
	for _, f := range []string{"tmux", "color", "width", "json"} {
		if c.Flags().Lookup(f) == nil {
			t.Errorf("status lacks --%s", f)
		}
	}
	a := storeApp(t)
	addTask(t, a, store.Task{ID: "t1", Title: "t1", Status: store.NeedsYou})
	t.Chdir(a.Root)
	t.Setenv("SADDLE_ROOT", a.Root)
	if err := os.WriteFile(a.Root+"/.saddle/config.toml", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	c.SetOut(&out)
	c.SetArgs([]string{"--tmux"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "0 run · 1 you · 0 queued · am off\n" {
		t.Errorf("status --tmux: %q", got)
	}
}
