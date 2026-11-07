package app

import (
	"os"
	"strings"
	"testing"
	"time"
)

// wakeAlarms drains t0's action notices and returns the wake-path alarms.
func wakeAlarms(t *testing.T, a *App) []string {
	t.Helper()
	var out []string
	for _, n := range actionNotices(t, a, OrchestratorID) {
		if strings.Contains(n, wakeAlarmMark) {
			out = append(out, n)
		}
	}
	return out
}

// staleBeat writes a heartbeat file last touched at when.
func staleBeat(t *testing.T, p string, when time.Time) {
	t.Helper()
	must(t, os.WriteFile(p, []byte("1\n"), 0o644))
	must(t, os.Chtimes(p, when, when))
}

// wakeAlarmApp is an app with a live worker, a stale mod heartbeat, no wait
// heartbeat and no hook activity: nothing would wake the orchestrator.
func wakeAlarmApp(t *testing.T) (*App, *ownerSink, time.Time) {
	t.Helper()
	a, _, sink := rotationApp(t)
	_, err := a.Spawn(SpawnReq{ID: "t1", Title: "work"})
	must(t, err)
	now := time.Now()
	must(t, os.MkdirAll(a.stateDir(), 0o755))
	staleBeat(t, a.ModHeartbeatPath(), now.Add(-time.Hour))
	_ = actionNotices(t, a, OrchestratorID)
	return a, sink, now
}

// #860: with no wait heartbeat, a stale mod heartbeat and no recent hook
// activity, a stall tick raises one alarm, to t0 and to the owner.
func TestStallTickAlarmsWhenEveryWakePathIsAbsent(t *testing.T) {
	a, sink, now := wakeAlarmApp(t)
	_, _ = a.StallTick(now)
	if got := wakeAlarms(t, a); len(got) != 1 {
		t.Fatalf("alarms after first tick = %q, want one", got)
	}
	told := 0
	for _, s := range sink.all() {
		if strings.Contains(s, wakeAlarmMark) {
			told++
		}
	}
	if told != 1 {
		t.Fatalf("owner told %d times, want 1: %q", told, sink.all())
	}
	// The same outage alarms once, not every cycle.
	_, _ = a.StallTick(now.Add(time.Minute))
	if got := wakeAlarms(t, a); len(got) != 0 {
		t.Fatalf("second tick alarmed again: %q", got)
	}
	// A path comes back, then goes away: that is a new outage.
	a.BeatWait(now.Add(2 * time.Minute))
	_, _ = a.StallTick(now.Add(2 * time.Minute))
	if got := wakeAlarms(t, a); len(got) != 0 {
		t.Fatalf("alarmed with a wait running: %q", got)
	}
	a.EndWait()
	_, _ = a.StallTick(now.Add(3 * time.Minute))
	if got := wakeAlarms(t, a); len(got) != 1 {
		t.Fatalf("alarms after the wait exited = %q, want one", got)
	}
}

// #860: any one wake path alive suppresses the alarm.
func TestStallTickNoWakeAlarmWhileOnePathIsAlive(t *testing.T) {
	for name, arm := range map[string]func(a *App, now time.Time){
		"wait": func(a *App, now time.Time) { a.BeatWait(now) },
		"mod":  func(a *App, now time.Time) { staleBeat(t, a.ModHeartbeatPath(), now) },
		"hook": func(a *App, now time.Time) { a.BeatHook(now.Add(-time.Minute)) },
	} {
		t.Run(name, func(t *testing.T) {
			a, sink, now := wakeAlarmApp(t)
			arm(a, now)
			_, _ = a.StallTick(now)
			if got := wakeAlarms(t, a); len(got) != 0 {
				t.Fatalf("alarmed with the %s path alive: %q", name, got)
			}
			for _, s := range sink.all() {
				if strings.Contains(s, wakeAlarmMark) {
					t.Fatalf("owner told with the %s path alive: %q", name, s)
				}
			}
		})
	}
}

// A repo where no orchestrator session ever ran (no heartbeat of any kind)
// isn't alarmed, nor is one with no worker at work.
func TestNoWakeAlarmWithoutAnOrchestratorOrWork(t *testing.T) {
	a, _, now := wakeAlarmApp(t)
	must(t, os.Remove(a.ModHeartbeatPath()))
	_, _ = a.StallTick(now)
	if got := wakeAlarms(t, a); len(got) != 0 {
		t.Fatalf("alarmed with no orchestrator session ever seen: %q", got)
	}

	b, _, _ := rotationApp(t)
	must(t, os.MkdirAll(b.stateDir(), 0o755))
	staleBeat(t, b.ModHeartbeatPath(), now.Add(-time.Hour))
	_, _ = b.StallTick(now)
	if got := wakeAlarms(t, b); len(got) != 0 {
		t.Fatalf("alarmed with no worker at work: %q", got)
	}
}
