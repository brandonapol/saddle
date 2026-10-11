package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/store"
)

func watchdogNotices(t *testing.T, a *App) []store.Notice {
	t.Helper()
	ns, err := a.Store.TakeNotices(OrchestratorID, false)
	must(t, err)
	var out []store.Notice
	for _, n := range ns {
		if strings.HasPrefix(n.Text, "‼ No progress") {
			out = append(out, n)
		}
	}
	return out
}

// #406 acceptance: with nothing landed, merged or started for idle_alarm
// while work waits, the watchdog posts exactly one card naming the single
// root cause, and doesn't repeat it until something changes.
func TestWatchdogReportsOneRootCause(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	now := time.Now().UTC()
	st := WatchStatus{
		LastProgress: now.Add(-50 * time.Minute),
		OpenTasks:    3,
		Live:         3,
		// Several symptoms at once: the root cause is the expired gh auth.
		GHAuth:        errors.New("gh: the token in keyring is invalid"),
		Adapters:      []agent.Status{{Name: "claude", OK: true, OutOfQuotaUntil: "2026-10-12T03:00:00Z"}},
		PromptBlocked: 3,
		Load1:         64, CPUs: 8,
	}
	rc, posted, err := a.Watchdog(now, st)
	must(t, err)
	if !posted || rc.Key != CauseGHAuth {
		t.Fatalf("root cause = %+v (posted %v), want gh auth", rc, posted)
	}
	ns := watchdogNotices(t, a)
	if len(ns) != 1 || !strings.Contains(ns[0].Text, "gh auth") || strings.Contains(ns[0].Text, "quota") {
		t.Fatalf("notices = %+v, want one card naming only gh auth", ns)
	}
	// No repeats while nothing changed.
	for _, d := range []time.Duration{time.Minute, time.Hour} {
		if _, posted, err := a.Watchdog(now.Add(d), st); err != nil || posted {
			t.Fatalf("repeat after %s: posted %v, %v", d, posted, err)
		}
	}
	if ns := watchdogNotices(t, a); len(ns) != 0 {
		t.Fatalf("repeated: %+v", ns)
	}
	// Progress resumes: the card closes.
	st.LastProgress = now.Add(2 * time.Hour)
	if _, posted, _ := a.Watchdog(now.Add(2*time.Hour), st); posted {
		t.Fatal("posted while progressing")
	}
	if open, _ := a.OpenDecisions(); len(open) != 0 {
		t.Fatalf("open cards after progress = %+v", open)
	}
}

func TestDiagnoseOneCause(t *testing.T) {
	t.Parallel()
	quota := []agent.Status{{Name: "claude", OK: true, OutOfQuotaUntil: "x"}, {Name: "grok", OK: true, OutOfQuotaUntil: "y"}}
	for _, tc := range []struct {
		name string
		st   WatchStatus
		want string
	}{
		{"quota", WatchStatus{Adapters: quota, PromptBlocked: 2, Live: 2}, CauseQuota},
		{"some quota left", WatchStatus{Adapters: append(quota, agent.Status{Name: "codex", OK: true}), PromptBlocked: 2, Live: 2}, CausePrompts},
		{"prompts", WatchStatus{PromptBlocked: 2, Live: 2, Load1: 99, CPUs: 4}, CausePrompts},
		{"overloaded", WatchStatus{PromptBlocked: 1, Live: 2, Load1: 99, CPUs: 4}, CauseOverload},
		{"unknown", WatchStatus{Live: 1, Load1: 1, CPUs: 4}, CauseUnknown},
	} {
		if got := Diagnose(tc.st); got.Key != tc.want {
			t.Errorf("%s: cause %q, want %q", tc.name, got.Key, tc.want)
		}
	}
}

// The watchdog stays quiet when nothing waits or progress is recent.
func TestWatchdogQuietWhenIdleOrRecent(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	now := time.Now().UTC()
	if _, posted, _ := a.Watchdog(now, WatchStatus{LastProgress: now.Add(-5 * time.Hour)}); posted {
		t.Fatal("posted with no work waiting")
	}
	if _, posted, _ := a.Watchdog(now, WatchStatus{LastProgress: now.Add(-10 * time.Minute), OpenTasks: 2}); posted {
		t.Fatal("posted inside idle_alarm")
	}
}
