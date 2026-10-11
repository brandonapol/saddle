package app

import (
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

func TestHeadlinePriority(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	task := func(id, status string) store.Task { return store.Task{ID: id, Status: status} }
	ask := map[string]store.Event{"t12": {TS: now.Add(-2 * time.Hour), Task: "t12", Kind: "notification", Data: "dismiss the GitGuardian alert"}}
	redFixing := CIRedState{Red: []CIRedLayer{{Task: "t20", PR: "https://github.com/o/r/pull/366", Since: now.Add(-30 * time.Minute), Repair: "t21"}}}
	redStuck := CIRedState{Red: []CIRedLayer{{Task: "t20", PR: "https://github.com/o/r/pull/366", Since: now.Add(-2 * time.Hour), Held: []string{"t22", "t23"}}}}
	cases := []struct {
		name, class, color, text string
		in                       HeadlineInput
	}{
		{
			name: "idle", class: HeadlineIdle, color: ColorGrey,
			text: "Idle: no ready work. Add issues or run `saddle autopilot on`.",
			in:   HeadlineInput{Tasks: []store.Task{task(OrchestratorID, store.Running), task("t1", store.Landed), task("t2", store.Killed)}},
		},
		{
			name: "idle under autopilot", class: HeadlineIdle, color: ColorGrey,
			text: "Idle: autopilot is on and waiting for ready work.",
			in:   HeadlineInput{Autopilot: true},
		},
		{
			name: "working", class: HeadlineWorking, color: ColorGreen,
			text: "Working: 3 agents running, 1 branch queued to land, 2 PRs merging. Nothing needs you.",
			in: HeadlineInput{
				Tasks:   []store.Task{task("t1", store.Running), task("t2", store.Running), task("t3", store.Idle), task("t4", store.Done)},
				Train:   []store.TrainEntry{{Task: "t4", State: store.Queued}, {Task: "t5", State: store.TrainOK}},
				Merging: 2,
			},
		},
		{
			name: "usage warn is still working", class: HeadlineWorking, color: ColorGreen,
			text: "Working: 1 agent running. Nothing needs you.",
			in:   HeadlineInput{Tasks: []store.Task{task("t1", store.Running)}, Usage: usage.Warn},
		},
		{
			name: "degraded: ci red with a fix", class: HeadlineDegraded, color: ColorYellow,
			text: "Slowed: CI is red on t20 (#366) and a fix is running. Nothing needs you yet.",
			in:   HeadlineInput{Tasks: []store.Task{task("t1", store.Running)}, CIRed: redFixing},
		},
		{
			name: "degraded: usage, adapter, conflict", class: HeadlineDegraded, color: ColorYellow,
			text: "Slowed: the plan limit is used up (5h 104%); codex is out of quota until 17:20; and 1 more. Nothing needs you yet.",
			in: HeadlineInput{
				Tasks:     []store.Task{task("t1", store.Conflict)},
				Usage:     usage.Over,
				UsageNote: "5h 104%",
				Exhausted: map[string]Exhaustion{"codex": {Adapter: "codex", Resets: "17:20"}},
			},
		},
		{
			name: "blocked beats degraded", class: HeadlineBlocked, color: ColorRed,
			text: "Blocked 2h: CI is red on t20 (#366) and no fix is running; 2 layers above it are waiting.",
			in:   HeadlineInput{Tasks: []store.Task{task("t1", store.Conflict)}, CIRed: redStuck, Usage: usage.Over},
		},
		{
			name: "blocked: stack at risk and auto-merge stopped", class: HeadlineBlocked, color: ColorRed,
			text: "Blocked: the stack is at risk at t7 (restack conflict); run saddle restack; auto-merge stopped: merge failed.",
			in:   HeadlineInput{Flag: &StackFlag{Task: "t7", Cause: "restack conflict"}, AutomergeStopped: "merge failed"},
		},
		{
			name: "acked flag and acked red hold nothing", class: HeadlineIdle, color: ColorGrey,
			text: "Idle: no ready work. Add issues or run `saddle autopilot on`.",
			in:   HeadlineInput{Flag: &StackFlag{Task: "t7", Acked: true}, CIRed: CIRedState{Red: []CIRedLayer{{Task: "t20", Acked: true}}}},
		},
		{
			name: "needs-you beats blocked", class: HeadlineNeedsYou, color: ColorRed,
			text: "Needs you 2h: t12 (#366) asks: dismiss the GitGuardian alert.",
			in: HeadlineInput{
				Tasks: []store.Task{{ID: "t12", Status: store.NeedsYou, PR: "https://github.com/o/r/pull/366"}, task("t1", store.Running)},
				Asks:  ask, CIRed: redStuck, AutomergeStopped: "merge failed",
			},
		},
		{
			name: "needs-you holding layers", class: HeadlineNeedsYou, color: ColorRed,
			text: "Needs you 2h: t20 (#366) asks: dismiss the GitGuardian alert. 2 layers above it are waiting.",
			in: HeadlineInput{
				Tasks: []store.Task{{ID: "t20", Status: store.NeedsYou, PR: "366"}},
				Asks:  map[string]store.Event{"t20": ask["t12"]}, CIRed: redStuck,
			},
		},
		{
			name: "several need you, oldest first", class: HeadlineNeedsYou, color: ColorRed,
			text: "Needs you 2h: 2 agents are waiting on you (t12 (#366), t13).",
			in: HeadlineInput{
				Tasks: []store.Task{task("t13", store.NeedsYou), {ID: "t12", Status: store.NeedsYou, Issue: 366}},
				Asks:  ask,
			},
		},
		{
			name: "needs-you without a notification", class: HeadlineNeedsYou, color: ColorRed,
			text: "Needs you: t13 is waiting at a prompt.",
			in:   HeadlineInput{Tasks: []store.Task{task("t13", store.NeedsYou)}},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.in.Now = now
			h := Headline(c.in)
			if h.Class != c.class || h.Color != c.color || h.Text != c.text {
				t.Fatalf("Headline() = %+v\nwant {Class:%s Color:%s Text:%s}", h, c.class, c.color, c.text)
			}
		})
	}
}

func TestHeadlineClipsLongAsks(t *testing.T) {
	now := time.Now()
	in := HeadlineInput{
		Now:   now,
		Tasks: []store.Task{{ID: "t1", Status: store.NeedsYou}},
		Asks:  map[string]store.Event{"t1": {TS: now, Data: strings.Repeat("very long question ", 20)}},
	}
	h := Headline(in)
	if len([]rune(h.Text)) > 120 || !strings.Contains(h.Text, "…") {
		t.Fatalf("long ask not clipped: %q", h.Text)
	}
}

func TestAppHeadlineReadsState(t *testing.T) {
	a, _ := setup(t)
	h, err := a.Headline(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if h.Class != HeadlineIdle {
		t.Fatalf("fresh repo headline = %+v, want idle", h)
	}
	tk := store.Task{ID: "t1", Title: "x", Role: store.RoleWorker, Status: store.NeedsYou}
	if err := a.Store.CreateTask(tk); err != nil {
		t.Fatal(err)
	}
	a.Store.Event("t1", "notification", "allow rm -rf build?")
	h, err = a.Headline(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if h.Class != HeadlineNeedsYou || !strings.Contains(h.Text, "allow rm -rf build?") {
		t.Fatalf("headline = %+v, want t1's ask", h)
	}
}
