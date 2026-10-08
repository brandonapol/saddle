package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

func readyQueue(issues ...autopilot.Issue) ReadyIssues {
	return func(string) ([]autopilot.Issue, error) { return issues, nil }
}

func apIssue(n int, body string) autopilot.Issue {
	return autopilot.Issue{Number: n, Title: "issue " + string(rune('a'+n)), Body: body}
}

// #256: a tick spawns ready issues as tasks with their claims, model scope,
// issue number and a prompt carrying AGENTS.md, up to the concurrency cap.
func TestAutopilotSpawnsReadyIssues(t *testing.T) {
	a, _ := setup(t)
	write(t, a.Root, "AGENTS.md", "# Agent notes\nNo AI co-authors.\n")
	_, err := a.SetConcurrency(2)
	must(t, err)
	d := a.NewAutopilot(readyQueue(
		apIssue(1, "claims: alpha/**"),
		apIssue(2, "**Model scope: Sonnet.**\nclaims: beta/**"),
		apIssue(3, "claims: gamma/**")))
	_, err = d.Enable(autopilot.Options{})
	must(t, err)
	rep, err := d.Tick()
	must(t, err)
	if len(rep.Spawned) != 2 {
		t.Fatalf("spawned %+v, want 2 (the cap)", rep.Spawned)
	}
	ts, err := a.Store.Tasks()
	must(t, err)
	all, err := a.Store.Claims()
	must(t, err)
	byIssue := map[int]store.Task{}
	for _, tk := range ts {
		byIssue[tk.Issue] = tk
	}
	t1, t2 := byIssue[1], byIssue[2]
	if t1.ID == "" || t2.ID == "" || byIssue[3].ID != "" {
		t.Fatalf("tasks by issue: %+v", byIssue)
	}
	if t1.Model != "opus" || t2.Model != "sonnet" {
		t.Errorf("models %q, %q; want opus and sonnet", t1.Model, t2.Model)
	}
	if !slices.Equal(all[t1.ID], []string{"alpha/**"}) {
		t.Errorf("claims of %s = %v", t1.ID, all[t1.ID])
	}
	if !strings.Contains(t1.Prompt, "No AI co-authors.") || !strings.Contains(t1.Prompt, "`true` passes") {
		t.Errorf("prompt lacks the repo rules or check:\n%s", t1.Prompt)
	}
	if n := countEvents(t, a, autopilot.EventSpawn, "#1"); n != 1 {
		t.Errorf("%d spawn events for #1", n)
	}
	// The next tick sees the cap and the tasks it started; nothing doubles.
	rep, err = d.Tick()
	must(t, err)
	if len(rep.Spawned) != 0 || !strings.Contains(rep.Skipped[3], "cap") {
		t.Fatalf("second tick: %+v", rep)
	}
}

// #256: a task whose window is gone and has been silent is resumed by the
// tick, so a dead task never holds a slot.
func TestAutopilotReconcilesOrphans(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	a, ft := setup(t)
	old := OrphanAfter
	OrphanAfter = 0
	t.Cleanup(func() { OrphanAfter = old })
	tk, err := a.Spawn(SpawnReq{Title: "one", Prompt: "do one", Claims: []string{"one/**"}})
	must(t, err)
	serverDies(ft)
	d := a.NewAutopilot(readyQueue())
	_, err = d.Enable(autopilot.Options{})
	must(t, err)
	rep, err := d.Tick()
	must(t, err)
	if !slices.Equal(rep.Resumed, []string{tk.ID}) {
		t.Fatalf("resumed %v, want %s", rep.Resumed, tk.ID)
	}
	if !a.ownWindow(mustTask(t, a, tk.ID)) {
		t.Fatal("no new window")
	}
}

// #256: a stall nudge goes through the orchestrator's safe-send target: never
// while it is busy or the owner is typing, and never as a notice.
func TestAutopilotNudgesThroughCompactTarget(t *testing.T) {
	a, _ := setup(t)
	var sent []string
	busy := true
	a.SetCompactTarget(FuncTarget{BusyFn: func() bool { return busy }, SendFn: func(s string) error { sent = append(sent, s); return nil }})
	t.Cleanup(func() { a.SetCompactTarget(nil) })
	// Both want alpha: the second waits on the first, below the cap.
	d := a.NewAutopilot(readyQueue(apIssue(1, "claims: alpha/**"), apIssue(2, "claims: alpha/x.go")))
	now := time.Now()
	d.Now = func() time.Time { return now }
	_, err := d.Enable(autopilot.Options{})
	must(t, err)
	for range 3 {
		_, err = d.Tick()
		must(t, err)
		now = now.Add(autopilot.DefaultStallAfter)
	}
	if len(sent) != 0 {
		t.Fatalf("nudged a busy orchestrator: %q", sent)
	}
	busy = false
	_, err = d.Tick()
	must(t, err)
	if len(sent) != 1 || !strings.HasPrefix(sent[0], "[saddle autopilot] continue") {
		t.Fatalf("sent %q", sent)
	}
	ns, err := a.Store.PeekNotices(OrchestratorID, false)
	must(t, err)
	if len(ns) != 0 {
		t.Errorf("the nudge was also queued as a notice: %+v", ns)
	}
}

// #256: plan-limit pressure (limits.pause_launches with a window over its
// cap) puts autopilot to sleep until the window resets.
func TestAutopilotSleepsOnPlanLimit(t *testing.T) {
	a, _ := setup(t)
	now := time.Now().Truncate(time.Minute)
	a.Cfg.Limits = usage.Limits{FiveHour: usage.Cap{Tokens: 100}, PauseLaunches: true}
	must(t, a.Store.AddUsage("s1", false, []usage.Bucket{{
		Key:    usage.Key{Minute: now.Add(-time.Hour), Task: "t9", Model: "claude-opus-4"},
		Tokens: usage.Tokens{Input: 500}, Messages: 1}}))
	d := a.NewAutopilot(readyQueue(apIssue(1, "claims: alpha/**")))
	d.Now = func() time.Time { return now }
	_, err := d.Enable(autopilot.Options{})
	must(t, err)
	rep, err := d.Tick()
	must(t, err)
	st, err := d.Status()
	must(t, err)
	want := now.Add(-time.Hour).Add(usage.FiveHours)
	if !rep.Sleeping || len(rep.Spawned) != 0 || !st.SleepUntil.Equal(want) {
		t.Fatalf("rep %+v, sleep until %v; want asleep until %v", rep, st.SleepUntil, want)
	}
	// Spawn checks limits against the wall clock, which the test can't move:
	// the window reset stands in for it.
	now = want
	a.Cfg.Limits.PauseLaunches = false
	rep, err = d.Tick()
	must(t, err)
	if rep.Sleeping || len(rep.Spawned) != 1 {
		t.Fatalf("after the reset: %+v", rep)
	}
}

// #256: the state file lives under .saddle/ and its stop summary reaches the
// orchestrator as an interrupt.
func TestAutopilotStopSummaryInterrupts(t *testing.T) {
	a, _ := setup(t)
	d := a.NewAutopilot(readyQueue())
	_, err := d.Enable(autopilot.Options{})
	must(t, err)
	_, err = d.Tick()
	must(t, err)
	if _, err := os.Stat(filepath.Join(a.Root, ".saddle", "autopilot.json")); err != nil {
		t.Fatal(err)
	}
	ns, err := a.Store.PeekNotices(OrchestratorID, false)
	must(t, err)
	if len(ns) != 1 || ns[0].Kind != store.NoticeAction || !strings.Contains(ns[0].Text, "autopilot stopped: queue empty") {
		t.Fatalf("notices = %+v", ns)
	}
}
