package app

import (
	"slices"
	"strings"
	"testing"
)

// #193: spawn records an explicit after edge so the task stacks on its
// dependency even when their files don't overlap.
func TestSpawnRecordsAfter(t *testing.T) {
	a := trainSetup(t)
	t1, err := a.Spawn(SpawnReq{ID: "t1", Title: "harness"})
	must(t, err)
	t2, err := a.Spawn(SpawnReq{ID: "t2", Title: "ci job", After: []string{t1.ID, " t1 "}})
	must(t, err)
	if got := a.TaskAfter(t2.ID); !slices.Equal(got, []string{"t1"}) {
		t.Fatalf("after = %v, want [t1]", got)
	}
	if got := a.TaskAfter(t1.ID); len(got) != 0 {
		t.Fatalf("t1 after = %v, want none", got)
	}
	evs, err := a.Store.Events(50)
	must(t, err)
	found := false
	for _, e := range evs {
		if e.Kind == EventDependsOn && e.Task == "t2" && e.Data == "t1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no %s event for t2: %+v", EventDependsOn, evs)
	}
}

func TestSpawnRejectsBadAfter(t *testing.T) {
	a := trainSetup(t)
	for name, after := range map[string][]string{
		"unknown": {"t9"},
		"self":    {"t1"},
	} {
		_, err := a.Spawn(SpawnReq{ID: "t1", Title: "x", After: after})
		if err == nil || !strings.Contains(err.Error(), "after") {
			t.Fatalf("%s: err = %v, want an after error", name, err)
		}
		if _, err := a.Store.Task("t1"); err == nil {
			t.Fatalf("%s: a refused spawn left a task row", name)
		}
	}
}
