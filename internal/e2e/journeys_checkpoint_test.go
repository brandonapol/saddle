//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/checkpoint"
	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/refguard"
)

// TestJourneyCheckpointsKeepWorkBetweenCommits (#50): two agents have edited
// files but not committed. The checkpoint watcher snapshots both worktrees
// into refs/saddle/checkpoints/<task> without moving their branches or
// tripping the ref guard, and nudges the agent with many dirty files to
// commit. It commits and lands, and its checkpoint is pruned; the other is
// killed, and its checkpoint is pruned once kill saved the work. No
// checkpoint ever reaches the remote.
func TestJourneyCheckpointsKeepWorkBetweenCommits(t *testing.T) {
	w := world(t, Options{})
	var steps []fa.Step
	for _, f := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		steps = append(steps, fa.Write("alpha/"+f+".txt", f+"\n"))
	}
	steps = append(steps, fa.Wait("go-on"), fa.Commit("alpha files"), fa.Done("Adds alpha files."))
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, steps...)
	w.Spawn("t2", "Beta work", []string{"beta/**"}, fa.Write("beta/wip.txt", "wip\n"), fa.Wait("never-comes"))
	w.WaitStatus("t1", "idle")
	w.WaitStatus("t2", "idle")

	a := w.App()
	tips := map[string]string{}
	for _, id := range []string{"t1", "t2"} {
		tips[id] = w.Git(w.Repo, "rev-parse", w.Task(id).Branch)
	}
	a.NewCheckpointWatcher().Tick()

	if got := w.Git(w.Repo, "show", checkpoint.Ref("t1")+":alpha/h.txt"); got != "h" {
		t.Fatalf("t1's checkpoint has alpha/h.txt = %q", got)
	}
	if got := w.Git(w.Repo, "show", checkpoint.Ref("t2")+":beta/wip.txt"); got != "wip" {
		t.Fatalf("t2's checkpoint has beta/wip.txt = %q", got)
	}
	for id, tip := range tips {
		if got := w.Git(w.Repo, "rev-parse", w.Task(id).Branch); got != tip {
			t.Fatalf("checkpointing moved %s's branch %s -> %s", id, tip, got)
		}
		if st := w.Git(w.Task(id).Worktree, "status", "--porcelain"); !strings.Contains(st, "??") {
			t.Fatalf("checkpointing touched %s's index:\n%s", id, st)
		}
	}
	es, err := a.Store.Events(500)
	must(t, err)
	for _, e := range es {
		if e.Kind == refguard.KindDenied {
			t.Fatalf("the ref guard denied something: %+v", e)
		}
	}
	ns, err := a.Store.PeekNotices("t1", false)
	must(t, err)
	if len(ns) != 1 || !strings.Contains(ns[0].Text, "Commit a small, coherent unit") {
		t.Fatalf("t1 was not nudged to commit: %+v", ns)
	}
	if ns, _ := a.Store.PeekNotices("t2", false); len(ns) != 0 {
		t.Fatalf("t2, with one dirty file, was nudged: %+v", ns)
	}

	w.MustSaddle("message", "t1", "go-on")
	w.WaitTask("t1", "queued in the train", queued)
	w.MustSaddle("land")
	w.WaitStatus("t1", "landed")
	if c, err := checkpoint.Lookup(w.Repo, "t1"); err == nil {
		t.Fatalf("landed t1 kept checkpoint %s", c)
	}

	w.MustSaddle("kill", "t2")
	if c, err := checkpoint.Lookup(w.Repo, "t2"); err == nil {
		t.Fatalf("killed t2 kept checkpoint %s", c)
	}
	if got := w.Git(w.Repo, "show", "refs/saddle/wip/t2:beta/wip.txt"); got != "wip" {
		t.Fatalf("kill lost t2's uncommitted work: %q", got)
	}
	if remote := w.Git(w.Repo, "ls-remote", "origin"); strings.Contains(remote, "refs/saddle/") {
		t.Fatalf("saddle refs reached the remote:\n%s", remote)
	}
}
