package app

import (
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

func TestCatchUpOrdersNeedsYouFirstAndCapsAtFive(t *testing.T) {
	ts := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	ev := func(task, kind, data string) store.Event {
		ts = ts.Add(time.Minute)
		return store.Event{TS: ts, Task: task, Kind: kind, Data: data}
	}
	es := []store.Event{
		ev("t10", "spawn", ""),
		ev("t11", "spawn", ""),
		ev("t1", "landed", ""),
		ev("t2", "landed", ""),
		ev("t3", "landed", ""),
		ev("t4", "landed", ""),
		ev("", "automerge_merged", "#360"),
		ev("t5", "train_conflict", "internal/x.go"),
		ev("t6", "ci_red", "lint"),
		ev("t9", "train_conflict", "since landed"),
		ev("t9", "landed", ""),
		ev("t7", "notification", "allow git push?"),
		ev("t8", "notification", "answered already"),
		ev("t12", "pre_tool", "noise"),
	}
	tasks := []store.Task{
		{ID: "t7", Status: store.NeedsYou, PR: "https://github.com/o/r/pull/366"},
		{ID: "t8", Status: store.Running},
		{ID: "t9", Status: store.Landed},
	}
	now := StateHeadline{Class: HeadlineNeedsYou, Text: "Needs you: t7 (#366) asks: allow git push?"}
	got := CatchUp(es, tasks, now)
	want := []string{
		"Waiting on you: t7 (#366) asks: allow git push?",
		"Stuck: t5 hit a conflict; t6 CI went red (lint).",
		"Done: 5 landed (t1, t2, t3 +2); 1 PR merged.",
		"Started: t10, t11.",
		"Now: Needs you: t7 (#366) asks: allow git push?",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("CatchUp() =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if len(got) > maxCatchUp {
		t.Fatalf("%d bullets, want at most %d", len(got), maxCatchUp)
	}
}

func TestCatchUpQuiet(t *testing.T) {
	got := CatchUp(nil, nil, StateHeadline{Text: "Idle: no ready work."})
	want := []string{"Nothing new since you last looked.", "Now: Idle: no ready work."}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("CatchUp() = %q, want %q", got, want)
	}
}

func TestCatchUpManyAsks(t *testing.T) {
	var es []store.Event
	var tasks []store.Task
	for _, id := range []string{"t1", "t2", "t3", "t4"} {
		es = append(es, store.Event{Task: id, Kind: "notification", Data: "question"})
		tasks = append(tasks, store.Task{ID: id, Status: store.NeedsYou})
	}
	got := CatchUp(es, tasks, StateHeadline{Text: "x"})
	if got[0] != "Waiting on you: 4 agents (t1, t2, t3 +1)." {
		t.Fatalf("first bullet = %q", got[0])
	}
}

func TestAppCatchUpSinceAndLastLooked(t *testing.T) {
	a, _ := setup(t)
	if !a.LastLooked().IsZero() {
		t.Fatal("LastLooked set before any look")
	}
	a.Store.Event("t1", "landed", "")
	// Event timestamps have second resolution: look after it.
	looked := time.Now().Add(time.Second)
	if err := a.MarkLooked(looked); err != nil {
		t.Fatal(err)
	}
	if got := a.LastLooked(); got.Unix() != looked.Unix() {
		t.Fatalf("LastLooked() = %v, want %v", got, looked)
	}
	bs, err := a.CatchUp(a.LastLooked(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(bs, "\n"), "landed") {
		t.Fatalf("catch-up since the last look lists an older landing: %q", bs)
	}
	bs, err = a.CatchUp(time.Now().Add(-time.Hour), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if bs[0] != "Done: 1 landed (t1)." {
		t.Fatalf("catch-up over the last hour = %q", bs)
	}
}
