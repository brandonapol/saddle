package app

import (
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// #675: after records an edge once the task is spawned, in the same deps
// file spawn writes, and refuses unknown tasks, self edges and cycles.
func TestAfterRecordsEdgePostSpawn(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	for _, id := range []string{"t1", "t2", "t3"} {
		_, err := a.Spawn(SpawnReq{ID: id, Title: id})
		must(t, err)
	}
	added, err := a.After("t2", []string{"t1", " t1 ", "t3"})
	must(t, err)
	if !slices.Equal(added, []string{"t1", "t3"}) || !slices.Equal(a.TaskAfter("t2"), []string{"t1", "t3"}) {
		t.Fatalf("added %v, after %v; want [t1 t3]", added, a.TaskAfter("t2"))
	}
	if added, err := a.After("t2", []string{"t1"}); err != nil || len(added) != 0 {
		t.Fatalf("re-adding t1: added %v, err %v; want nothing new", added, err)
	}
	evs, err := a.Store.Events(50)
	must(t, err)
	if !slices.ContainsFunc(evs, func(e store.Event) bool {
		return e.Kind == EventDependsOn && e.Task == "t2" && strings.HasPrefix(e.Data, "t1 (")
	}) {
		t.Fatalf("no %s event for t2 after t1: %+v", EventDependsOn, evs)
	}
	for name, c := range map[string]struct {
		task string
		deps []string
	}{
		"unknown task": {"t9", []string{"t1"}},
		"unknown dep":  {"t1", []string{"t9"}},
		"self":         {"t1", []string{"t1"}},
		"cycle":        {"t1", []string{"t2"}},
		"none":         {"t1", nil},
	} {
		if _, err := a.After(c.task, c.deps); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if got := a.TaskAfter("t1"); len(got) != 0 {
		t.Fatalf("a refused edge was written: t1 after %v", got)
	}
}

// #675: t2 uses a symbol t1 added, but their files don't overlap, so prs
// puts t1 in its own stack and t2's layer fails at its tip. The gate error
// names the probable missing edge and the exact command; once it is run,
// t2 stacks on t1 and prs goes through.
func TestPRsGateSuggestsMissingAfterEdge(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	_, ghLog := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "thing api", map[string]string{"api.go": "package p\n\nfunc NewThing() {}\n"})
	t2 := landTask(t, a, "t2", "use thing", map[string]string{"use.go": "package p\n\nfunc use() { NewThing() }\n"})
	a.Cfg.Train.Prepublish.Cmd = "test ! -f use.go || test -f api.go || { echo './use.go:3:14: undefined: NewThing'; exit 1; }"

	_, err := a.PRs()
	if err == nil {
		t.Fatal("prs published t2, whose tip lacks t1's NewThing")
	}
	for _, want := range []string{"layer t2", "t1 adds symbol NewThing", "saddle after t2 t1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if got := a.TaskAfter("t2"); len(got) != 0 {
		t.Fatalf("prs wrote an edge by itself: t2 after %v", got)
	}
	s, err := a.Gate()
	must(t, err)
	if len(s.Red) != 1 || !strings.Contains(s.Red[0].Hint, "saddle after t2 t1") {
		t.Fatalf("gate state = %+v, want t2 red with the after hint", s.Red)
	}

	_, err = a.After("t2", []string{"t1"})
	must(t, err)
	if _, err := a.PRs(); err != nil {
		t.Fatalf("prs after recording the edge: %v", err)
	}
	if bases := prBases(ghLog()); bases[t2.Branch] != t1.Branch {
		t.Fatalf("PR bases = %v, want t2 on %s", bases, t1.Branch)
	}
}

// A red layer no sibling explains gets no hint.
func TestGateAfterHintNeedsAnExplainingSibling(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	if h := a.afterHint("t1", "./x.go:1:1: undefined: Nope"); h != "" {
		t.Fatalf("hint = %q, want none", h)
	}
}
