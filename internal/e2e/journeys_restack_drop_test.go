//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// buildCmd is the fixture repo's "go build ./...": every program under app/
// must run, and app/main.sh needs lib/eventbus.sh, which another task adds.
const buildCmd = `set -e; for f in app/*.sh; do [ -e "$f" ] || continue; sh -eu "$f"; done`

// TestJourneyRestackKeepsUnpublishedLanded reproduces #219: t1 adds the
// event bus and t2 uses it; both land, neither has a PR, because prs is
// blocked by GitHub's native-stack base-change error. The orchestrator
// closes t1's finished agent (kill), and a teammate moves main. Restack must
// keep both tasks on integration, and integration must still build. Before
// the fix the kill made t1 "superseded", so restack dropped its commit and
// left integration with t2's caller but no event bus.
func TestJourneyRestackKeepsUnpublishedLanded(t *testing.T) {
	w := world(t, Options{Tables: "[train]\noutput = \"single\"\n", TestCmd: buildCmd})
	w.Spawn("t1", "Event bus", []string{"lib/**"},
		fa.Write("lib/eventbus.sh", "EVENT_RESYNC=resync\n"), fa.Commit("event bus"), fa.Done("Adds the event bus."))
	w.WaitTask("t1", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	w.MustSaddle("land")
	w.Spawn("t2", "Resync on index change", []string{"app/**"},
		fa.Write("app/main.sh", ". lib/eventbus.sh\necho \"$EVENT_RESYNC\"\n"), fa.Commit("use EventResync"), fa.Done("Uses the event bus."))
	w.WaitTask("t2", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	w.MustSaddle("land")
	for _, id := range []string{"t1", "t2"} {
		if v := w.Task(id); v.Status != "landed" || v.PR != "" {
			t.Fatalf("%s = %+v, want landed with no PR", id, v)
		}
	}
	landedTip := w.Git(w.Repo, "rev-parse", "saddle/integration")

	// prs is blocked: GitHub refuses to retarget a PR in a native stack.
	a := w.App()
	must(t, a.SetFlag(app.StackFlag{Task: "t1", Cause: "Cannot change the base branch because the pull request is part of a stack"}))
	if r := w.Saddle("prs"); r.Code == 0 {
		t.Fatalf("prs went ahead on a broken stack: %s", r)
	}
	// The orchestrator closes t1's finished agent, as it does to free a window.
	w.MustSaddle("kill", "t1")
	// A teammate merges an unrelated PR on main.
	w.WriteFile("docs/teammate.md", "teammate\n")
	w.Git(w.Repo, "add", "docs")
	w.Git(w.Repo, "commit", "-q", "-m", "teammate change (#300)")
	w.Git(w.Repo, "push", "-q", "origin", "main")

	w.MustMCP("t0", "restack", nil)
	files := w.Git(w.Repo, "ls-tree", "-r", "--name-only", "saddle/integration")
	for _, f := range []string{"lib/eventbus.sh", "app/main.sh", "docs/teammate.md"} {
		if !strings.Contains(files, f) {
			t.Errorf("integration lost %s after restack:\n%s", f, files)
		}
	}
	for _, id := range []string{"t1", "t2"} {
		if v := w.Task(id); strings.HasPrefix(v.Train, app.TrainSuperseded) {
			t.Errorf("%s left the stack: %+v", id, v)
		}
	}
	if r := w.Exec(w.Repo, "sh", "-c", "git worktree add -q --detach ../integ-check saddle/integration && cd ../integ-check && "+buildCmd); r.Code != 0 {
		t.Fatalf("integration doesn't build after restack: %s", r)
	}
	// The old tip is kept as a backup ref, so recovery is one command.
	if refs := w.Git(w.Repo, "for-each-ref", "--format=%(objectname)", "refs/saddle/integration-backups/"); !strings.Contains(refs, landedTip) {
		t.Fatalf("no backup of the pre-restack tip %s: %q", landedTip, refs)
	}
}
