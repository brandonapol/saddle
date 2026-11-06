package app

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// stallWorker spawns one running worker, so work waits.
func stallWorker(t *testing.T) (*App, *fakeTmux, store.Task) {
	t.Helper()
	a, ft := setup(t)
	tk, err := a.Spawn(SpawnReq{Title: "meter", Prompt: "build the meter", Claims: []string{"billing/**"}})
	must(t, err)
	must(t, a.Store.SetStatus(tk.ID, store.Running))
	tk, err = a.Store.Task(tk.ID)
	must(t, err)
	return a, ft, tk
}

// #783: one land, then more noise than the watchdog used to read (2001
// ci_error events), with a task open. The watchdog still sees the stall,
// and while it lasts it posts again after 1h, then 2h.
func TestWatchdogSeesStallUnderEventNoiseAndReposts(t *testing.T) {
	t.Parallel()
	a, _, _ := stallWorker(t)
	a.Store.Event("", "landed", "t9")
	for range 2001 {
		a.Store.Event("", "ci_error", "gh: 502")
	}
	now := time.Now().UTC().Add(time.Hour)
	st, err := a.watchStatus()
	must(t, err)
	if st.LastProgress.IsZero() {
		t.Fatal("the last progress fell out of view under event noise")
	}
	if _, posted, err := a.Watchdog(now, st); err != nil || !posted {
		t.Fatalf("no card for a stall under noise: posted %v, %v", posted, err)
	}
	_ = watchdogNotices(t, a)
	for _, tc := range []struct {
		at   time.Duration
		want bool
	}{
		{30 * time.Minute, false},
		{time.Hour, true},                  // 1h after the first
		{2*time.Hour + time.Minute, false}, // 1h after the second: inside its 2h
		{3 * time.Hour, true},              // 2h after the second
	} {
		st, err := a.watchStatus()
		must(t, err)
		_, posted, err := a.Watchdog(now.Add(tc.at), st)
		must(t, err)
		if posted != tc.want {
			t.Fatalf("at +%s: posted %v, want %v", tc.at, posted, tc.want)
		}
	}
	if ns := watchdogNotices(t, a); len(ns) != 2 {
		t.Fatalf("re-posts = %d notices, want 2", len(ns))
	}
	if open := openCardsOf(t, a, DecisionNoProgress); len(open) != 1 {
		t.Fatalf("open watchdog cards = %+v, want only the latest", open)
	}
}

// #783: a zombie (its worktree gone) is a named cause, and the card's
// default rescues it instead of leaving it.
func TestWatchdogDefaultRescuesZombie(t *testing.T) {
	t.Parallel()
	a, _, tk := stallWorker(t)
	must(t, os.RemoveAll(tk.Worktree))
	now := time.Now().UTC().Add(time.Hour)
	st, err := a.watchStatus()
	must(t, err)
	st.Zombies, st.Windowless, st.GaveUp = a.stalledTasks()
	rc, posted, err := a.Watchdog(now, st)
	must(t, err)
	if !posted || rc.Key != CauseZombie || !strings.Contains(rc.What, tk.ID) {
		t.Fatalf("cause = %+v (posted %v), want the zombie %s", rc, posted, tk.ID)
	}
	cards := openCardsOf(t, a, DecisionNoProgress)
	if len(cards) != 1 || cards[0].DefaultOption().Run != RunRescueStalled {
		t.Fatalf("cards = %+v, want one whose default rescues", cards)
	}
	if _, err := a.ExpireDecisions(now.Add(DefaultDecisionTimeout), a.runDefault); err != nil {
		t.Fatal(err)
	}
	got, err := a.Store.Task(tk.ID)
	must(t, err)
	if got.Status != store.Killed {
		t.Fatalf("zombie %s is %s after the default, want killed (rescued)", tk.ID, got.Status)
	}
}

// #783: a live task with no window is a named cause, and the card's
// default gives it a new one.
func TestWatchdogDefaultResumesWindowless(t *testing.T) {
	t.Parallel()
	a, ft, tk := stallWorker(t)
	must(t, ft.KillWindow(tk.Window))
	now := time.Now().UTC().Add(time.Hour)
	st, err := a.watchStatus()
	must(t, err)
	st.Zombies, st.Windowless, st.GaveUp = a.stalledTasks()
	rc, _, err := a.Watchdog(now, st)
	must(t, err)
	if rc.Key != CauseNoWindow {
		t.Fatalf("cause = %+v, want no-window", rc)
	}
	if _, err := a.ExpireDecisions(now.Add(DefaultDecisionTimeout), a.runDefault); err != nil {
		t.Fatal(err)
	}
	got, err := a.Store.Task(tk.ID)
	must(t, err)
	if got.Window == tk.Window || !ft.Alive(got.Window) {
		t.Fatalf("window after the default = %q (was %q), want a new live one", got.Window, tk.Window)
	}
}

// A task the hung watch gave up on is a named cause too.
func TestDiagnoseNamesStalledTasks(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		st   WatchStatus
		want string
		run  string
	}{
		{WatchStatus{Live: 2, PromptBlocked: 2, Zombies: []string{"t4"}}, CauseZombie, RunRescueStalled},
		{WatchStatus{Live: 2, PromptBlocked: 2, Windowless: []string{"t4"}}, CauseNoWindow, RunResumeLost},
		{WatchStatus{Live: 2, GaveUp: []string{"t4"}}, CauseGaveUp, RunRescueStalled},
		{WatchStatus{Live: 2, PromptBlocked: 2, GaveUp: []string{"t4"}}, CausePrompts, ""},
	} {
		if got := Diagnose(tc.st); got.Key != tc.want || got.Run != tc.run {
			t.Errorf("%+v: cause %+v, want %s running %q", tc.st, got, tc.want, tc.run)
		}
	}
}
