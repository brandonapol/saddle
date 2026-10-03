package app

import (
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/tmux"
)

// idleWorker spawns a Claude worker, marks it idle and puts screen in its pane.
func idleWorker(t *testing.T, a *App, ft *fakeTmux, screen string) store.Task {
	t.Helper()
	c, err := a.Spawn(SpawnReq{Title: "work"})
	must(t, err)
	must(t, a.Store.SetStatus(c.ID, store.Idle))
	ft.setScreen(c.Window, screen)
	c, err = a.Store.Task(c.ID)
	must(t, err)
	return c
}

func wakeEvents(t *testing.T, a *App, task string) []store.Event {
	t.Helper()
	es, err := a.Store.Events(200)
	must(t, err)
	var out []store.Event
	for _, e := range es {
		if e.Kind == "notice_wake" && e.Task == task {
			out = append(out, e)
		}
	}
	return out
}

func countWakes(sent []string) int {
	n := 0
	for _, s := range sent {
		if s == agent.WakeLine {
			n++
		}
	}
	return n
}

// #183: a notice for an idle agent whose input box shows Claude Code's grey
// suggestion is typed in at once.
func TestNotifyReachesIdleAgentShowingSuggestion(t *testing.T) {
	a, ft := setup(t)
	c := idleWorker(t, a, ft, "done\n\x1b[39m❯ \x1b[2mrun saddle restack now\x1b[0m")
	a.Tmux = styledFake{ft}
	must(t, a.Notify(c.ID, store.NoticeAction, "tests failed on the train"))
	if n := countWakes(ft.sentTo(c.Window)); n != 1 {
		t.Fatalf("wake line typed %d times over a suggestion, want 1: %q", n, ft.sentTo(c.Window))
	}
}

// styledFake hands out the fake's screen as a capture with escapes.
type styledFake struct{ *fakeTmux }

func (s styledFake) CaptureStyled(id string, n int) (string, error) { return s.Capture(id, n) }

// #183: notices waiting on an agent that has sat idle for wake_after are
// typed in even though its input looks like a draft, once per wake_after,
// with a notice_wake event.
func TestWakeIdleAfterWakeAfter(t *testing.T) {
	a, ft := setup(t)
	fastRetry(t)
	c := idleWorker(t, a, ft, "done\n❯ run saddle restack now")
	must(t, a.Notify(c.ID, store.NoticeAction, "tests failed on the train"))
	if n := countWakes(ft.sentTo(c.Window)); n != 0 {
		t.Fatalf("typed over a plain-capture draft at once: %q", ft.sentTo(c.Window))
	}
	now := time.Now()
	woke, err := a.WakeIdle(now)
	must(t, err)
	if len(woke) != 0 {
		t.Fatalf("woke %v before wake_after", woke)
	}
	woke, err = a.WakeIdle(now.Add(a.Cfg.Notices.WakeAfter + time.Second))
	must(t, err)
	if len(woke) != 1 || woke[0] != c.ID || countWakes(ft.sentTo(c.Window)) != 1 {
		t.Fatalf("woke %v, sent %q; want one wake for %s", woke, ft.sentTo(c.Window), c.ID)
	}
	es := wakeEvents(t, a, c.ID)
	if len(es) != 1 || !strings.Contains(es[0].Data, "draft") {
		t.Fatalf("notice_wake events %+v", es)
	}
	// Not again until another wake_after has passed.
	woke, _ = a.WakeIdle(now.Add(a.Cfg.Notices.WakeAfter + 2*time.Second))
	if len(woke) != 0 {
		t.Fatalf("woke again at once: %v", woke)
	}
	woke, _ = a.WakeIdle(now.Add(2*a.Cfg.Notices.WakeAfter + 2*time.Second))
	if len(woke) != 1 {
		t.Fatalf("not woken again after another wake_after: %v", woke)
	}
}

// The wake never answers a permission prompt, and never types at a busy
// agent or one with nothing pending.
func TestWakeIdleLeavesPromptsAndBusyAgentsAlone(t *testing.T) {
	a, ft := setup(t)
	fastRetry(t)
	later := time.Now().Add(a.Cfg.Notices.WakeAfter + time.Minute)

	prompt := idleWorker(t, a, ft, "Do you want to proceed?\n❯ 1. Yes\n  2. No\nEsc to cancel")
	must(t, a.Store.Notify(prompt.ID, store.NoticeAction, "x"))
	busy := idleWorker(t, a, ft, "working\n❯ ")
	must(t, a.Store.SetStatus(busy.ID, store.Running))
	must(t, a.Store.Notify(busy.ID, store.NoticeAction, "x"))
	info := idleWorker(t, a, ft, "done\n❯ ")
	must(t, a.Store.Notify(info.ID, store.NoticeInfo, "x"))

	woke, err := a.WakeIdle(later)
	must(t, err)
	if len(woke) != 0 {
		t.Fatalf("woke %v", woke)
	}
	for _, c := range []store.Task{prompt, busy, info} {
		if s := ft.sentTo(c.Window); len(s) != 0 {
			t.Errorf("%s: typed %q", c.ID, s)
		}
	}
}

func fastRetry(t *testing.T) {
	e, f, ce, cf := tmux.RetryEvery, tmux.RetryFor, tmux.ConfirmEvery, tmux.ConfirmFor
	tmux.RetryEvery, tmux.RetryFor = 5*time.Millisecond, 30*time.Millisecond
	tmux.ConfirmEvery, tmux.ConfirmFor = 5*time.Millisecond, 200*time.Millisecond
	t.Cleanup(func() { tmux.RetryEvery, tmux.RetryFor, tmux.ConfirmEvery, tmux.ConfirmFor = e, f, ce, cf })
}
