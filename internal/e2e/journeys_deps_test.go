//go:build e2e

package e2e

import (
	"testing"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// TestJourneyExplicitDependencyStacks (#193): t1 adds a make target, and t2,
// spawned with after=[t1], adds the CI job that runs it. Their files don't
// overlap and they share no issue, yet t2's PR stacks on t1's instead of
// going up on main, where it would fail CI until t1 merged. t3, unrelated,
// still gets its own PR on main.
func TestJourneyExplicitDependencyStacks(t *testing.T) {
	w := world(t, Options{})
	w.Spawn("t1", "e2e harness", []string{"Makefile"},
		fa.Write("Makefile", "test/e2e:\n\tgo test ./e2e\n"), fa.Commit("make test/e2e"), fa.Done("Adds make test/e2e."))
	must(t, fa.Script{Steps: []fa.Step{
		fa.Write("ci/e2e.yml", "run: make test/e2e\n"), fa.Commit("e2e CI job"), fa.Done("Runs make test/e2e in CI."),
	}}.Save(w.Scripts, "t2"))
	w.MustMCP("t0", "spawn", map[string]any{
		"id": "t2", "title": "e2e ci job", "prompt": "Run make test/e2e in CI.", "claims": []string{"ci/**"}, "after": []string{"t1"},
	})
	w.Spawn("t3", "Gamma work", []string{"gamma/**"}, finished("gamma", "gamma\n")...)
	for _, id := range []string{"t1", "t2", "t3"} {
		w.WaitTask(id, "queued in the train", func(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" })
	}
	w.MustSaddle("land")
	w.MustSaddle("prs")

	base := func(id string) string {
		url := w.Task(id).PR
		if url == "" {
			t.Fatalf("%s has no PR", id)
		}
		return prNumber(t, w.GHState(), url).Base
	}
	if got, want := base("t2"), w.Task("t1").Branch; got != want {
		t.Fatalf("t2's PR targets %s, want t1's branch %s", got, want)
	}
	for _, id := range []string{"t1", "t3"} {
		if got := base(id); got != "main" {
			t.Fatalf("%s's PR targets %s, want main", id, got)
		}
	}
}
