package app

import (
	"slices"
	"strings"
	"testing"
)

// #193: spawn records an explicit after edge so the task stacks on its
// dependency even when their files don't overlap.
func TestSpawnRecordsAfter(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

const siblingDiff = `diff --git a/Makefile b/Makefile
index 1111111..2222222 100644
--- a/Makefile
+++ b/Makefile
@@ -3,0 +4,3 @@ test:
+.PHONY: test/e2e
+test/e2e: build
+	go test -tags e2e ./internal/e2e/...
diff --git a/internal/e2e/harness.go b/internal/e2e/harness.go
new file mode 100644
index 0000000..3333333
--- /dev/null
+++ b/internal/e2e/harness.go
@@ -0,0 +1,9 @@
+package e2e
+
+type Harness struct{}
+
+func NewHarness() *Harness { return &Harness{} }
+
+func (h *Harness) Drive(keys string) {}
+
+const DefaultWait = 3
`

// #193: a CI failure counts as explained by a sibling only when the
// sibling's diff clearly adds the missing make target, file or symbol.
func TestMissingAddedBy(t *testing.T) {
	t.Parallel()
	diff := parseDiff(siblingDiff)
	for name, tc := range map[string]struct {
		log  string
		want string // "" means not explained
	}{
		"make target":               {"make: *** No rule to make target 'test/e2e'.  Stop.", "make target test/e2e"},
		"make target backticks":     {"make: *** No rule to make target `test/e2e'.  Stop.", "make target test/e2e"},
		"other make target":         {"make: *** No rule to make target 'lint'.  Stop.", ""},
		"phony alone is no rule":    {"make: *** No rule to make target '.PHONY'.  Stop.", ""},
		"missing file":              {"open /home/runner/work/r/r/internal/e2e/harness.go: no such file or directory", "file internal/e2e/harness.go"},
		"missing other file":        {"open /x/internal/e2e/other.go: no such file or directory", ""},
		"undefined qualified":       {"internal/cli/up.go:12:5: undefined: e2e.NewHarness", "symbol e2e.NewHarness"},
		"undefined wrong package":   {"internal/cli/up.go:12:5: undefined: app.NewHarness", ""},
		"undefined same package":    {"internal/e2e/journeys_test.go:9:2: undefined: DefaultWait", "symbol DefaultWait"},
		"undefined other package":   {"internal/cli/up.go:9:2: undefined: DefaultWait", ""},
		"undefined no location":     {"undefined: DefaultWait", ""},
		"missing method":            {"internal/e2e/x_test.go:4:4: h.Drive undefined (type *e2e.Harness has no field or method Drive)", "method Harness.Drive"},
		"missing method wrong type": {"x.go:4:4: h.Drive undefined (type *Other has no field or method Drive)", ""},
		"unrelated failure":         {"--- FAIL: TestMeter\n    meter_test.go:12: got 1, want 2", ""},
	} {
		got, ok := missingAddedBy(tc.log, diff)
		if tc.want == "" {
			if ok {
				t.Errorf("%s: explained as %q, want not explained", name, got)
			}
			continue
		}
		if !ok || got != tc.want {
			t.Errorf("%s: got %q, %v, want %q", name, got, ok, tc.want)
		}
	}
}

const noE2ETarget = "make: *** No rule to make target 'test/e2e'.  Stop."

// #193: a stacked sibling on its own PR, or a live worker's unlanded
// branch, that adds what the log says is missing explains the failure; a
// layer already below the failing one in its PR stack does not.
func TestDependencyExplainsPendingSibling(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	landTask(t, a, "t1", "e2e harness", map[string]string{"Makefile": "test/e2e:\n\tgo test ./e2e\n"})
	landTask(t, a, "t2", "e2e ci job", map[string]string{"e2e.yml": "run: make test/e2e\n"})
	if sib, thing, ok := a.dependencyExplains("t2", noE2ETarget); !ok || sib != "t1" || thing != "make target test/e2e" {
		t.Fatalf("got %q %q %v, want t1 explaining make target test/e2e", sib, thing, ok)
	}
	if _, _, ok := a.dependencyExplains("t2", "make: *** No rule to make target 'lint'.  Stop."); ok {
		t.Fatal("explained a target no sibling adds")
	}

	// Spawned after t1, t4 stacks on it: t1's work is in its base.
	t4, err := a.Spawn(SpawnReq{ID: "t4", Title: "lint job", After: []string{"t1"}})
	must(t, err)
	write(t, t4.Worktree, "lint.yml", "run: make test/e2e\n")
	commitAll(t, t4.Worktree, "lint job")
	must(t, a.Done("t4", "lint job"))
	_, err = a.Land()
	must(t, err)
	if sib, _, ok := a.dependencyExplains("t4", noE2ETarget); ok {
		t.Fatalf("explained by %s, which is below t4 in its stack", sib)
	}

	// A live worker that hasn't landed yet adds the lint target.
	t5, err := a.Spawn(SpawnReq{ID: "t5", Title: "lint target"})
	must(t, err)
	write(t, t5.Worktree, "lint.mk", "lint:\n\tgolangci-lint run\n")
	commitAll(t, t5.Worktree, "lint target")
	if sib, thing, ok := a.dependencyExplains("t4", "make: *** No rule to make target 'lint'.  Stop."); !ok || sib != "t5" || thing != "make target lint" {
		t.Fatalf("got %q %q %v, want t5 explaining make target lint", sib, thing, ok)
	}
}
