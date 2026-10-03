//go:build e2e

package e2e

import (
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/e2e/fakegh"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// repairsOf lists the tasks titled as repairs of orig.
func repairsOf(w *World, orig string) []mcpserver.TaskView {
	var out []mcpserver.TaskView
	for _, v := range w.Status().Tasks {
		if strings.HasPrefix(v.Title, "Repair "+orig+" ") {
			out = append(out, v)
		}
	}
	return out
}

// TestJourneyOrphanedConflictRepairs reproduces #172 (t55, t21): t1 landed
// and its window closed, then a human PR on main changed the same file. The
// restack conflict has no live owner, so restack spawns exactly one repair
// task (t2) and finishes the rest. t2 resolves the conflict on its own
// branch and lands normally; t1 is superseded, its PR closed, and the stack
// checks clean with no human edit.
func TestJourneyOrphanedConflictRepairs(t *testing.T) {
	w := world(t, Options{Tables: "[train]\noutput = \"single\"\n"})
	w.Spawn("t1", "Shared edit", []string{"shared.txt"},
		fa.Write("shared.txt", "one\n"), fa.Commit("one"), fa.Done("shared.txt says one."))
	w.WaitTask("t1", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	w.MustSaddle("land")
	w.MustSaddle("prs")
	t1PR := w.Task("t1").PR
	a := w.App()
	if t1, _ := a.Store.Task("t1"); w.Tmux.Alive(t1.Window) {
		t.Fatal("t1's window outlived its landing (close_on_land)")
	}

	// A human PR (#164 in the incident) changes the same file on main.
	w.WriteFile("shared.txt", "human\n")
	w.Git(w.Repo, "add", "shared.txt")
	w.Git(w.Repo, "commit", "-q", "-m", "human edit (#164)")
	w.Git(w.Repo, "push", "-q", "origin", "main")
	if r := w.MustSaddle("sentinel", "check"); !strings.Contains(r.Stdout, "at risk") {
		t.Fatalf("main moving didn't flag the stack: %s", r)
	}
	_, _ = a.Store.TakeNotices("t0", false)

	// The repair agent resolves keeping both sides, as a real one would.
	must(t, fa.Script{Steps: []fa.Step{
		fa.Write("shared.txt", "human\none\n"), fa.Commit("one, on top of the human edit"), fa.Done("Re-lands t1 on main.")}}.Save(w.Scripts, "t2"))
	w.MustMCP("t0", "restack", nil)
	if rs := repairsOf(w, "t1"); len(rs) != 1 || rs[0].ID != "t2" {
		t.Fatalf("repair tasks = %+v, want exactly t2", rs)
	}
	if v := w.Task("t1"); !strings.HasPrefix(v.Train, app.TrainRepairing) {
		t.Fatalf("t1 = %+v, want repairing", v)
	}
	if n, _ := a.Store.PendingNotices("t1"); n != 0 {
		t.Fatal("the conflict went to t1, whose agent is gone")
	}
	ns, err := a.Store.TakeNotices("t0", false)
	must(t, err)
	about := 0
	for _, n := range ns {
		if strings.Contains(n.Text, "spawned t2") {
			about++
		}
	}
	if about != 1 {
		t.Fatalf("orchestrator notices about the repair = %d, want one: %+v", about, ns)
	}
	// Another restack spawns no second repair.
	w.MustMCP("t0", "restack", nil)
	if rs := repairsOf(w, "t1"); len(rs) != 1 {
		t.Fatalf("second restack: repair tasks = %+v", rs)
	}

	w.WaitTask("t2", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	w.MustSaddle("land")
	if v := w.Task("t2"); v.Status != "landed" {
		t.Fatalf("repair didn't land: %+v", v)
	}
	if v := w.Task("t1"); !strings.HasPrefix(v.Train, app.TrainSuperseded) {
		t.Fatalf("t1 after its repair landed = %+v, want superseded", v)
	}
	if got := w.Git(w.Repo, "show", "saddle/integration:shared.txt"); got != "human\none" {
		t.Fatalf("integration shared.txt = %q", got)
	}
	p := prNumber(t, w.GHState(), t1PR)
	if p.State != "CLOSED" || !slices.ContainsFunc(p.Comments, func(c fakegh.Comment) bool { return strings.Contains(c.Body, "t2") }) {
		t.Fatalf("t1's PR = %s with comments %+v, want closed naming t2", p.State, p.Comments)
	}
	if r := w.MustSaddle("sentinel", "check"); !strings.Contains(r.Stdout, "checks clean") {
		t.Fatalf("stack still flagged after the repair landed: %s", r)
	}
	w.MustSaddle("prs")
	if url := w.Task("t2").PR; url == "" || prNumber(t, w.GHState(), url).Base != "main" {
		t.Fatalf("repair PR %q isn't on main", url)
	}
}
