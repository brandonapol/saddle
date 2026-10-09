package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/checkpoint"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// #50: the watcher checkpoints a live worker's uncommitted work without
// moving its branch, and nudges it through its mailbox to commit.
func TestCheckpointWatcherSnapshotsAndNudges(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	w, err := a.Spawn(SpawnReq{Title: "work"})
	must(t, err)
	tip := git(t, a.Root, "rev-parse", w.Branch)
	for _, f := range []string{"a", "b", "c", "d", "e", "f", "g", "h"} {
		write(t, w.Worktree, f+".txt", f+"\n")
	}
	a.NewCheckpointWatcher().Tick()
	c, err := checkpoint.Lookup(a.Root, w.ID)
	if err != nil {
		t.Fatalf("no checkpoint: %v", err)
	}
	if got := git(t, a.Root, "show", c+":h.txt"); got != "h" {
		t.Fatalf("h.txt in checkpoint = %q", got)
	}
	if got := git(t, a.Root, "rev-parse", w.Branch); got != tip {
		t.Fatalf("branch moved %s -> %s", tip, got)
	}
	ns, err := a.Store.PeekNotices(w.ID, false)
	must(t, err)
	if len(ns) != 1 || !strings.Contains(ns[0].Text, "Commit") {
		t.Fatalf("notices = %+v", ns)
	}
}

// #50: landing and killing a task prune its checkpoint.
func TestLandAndKillPruneCheckpoints(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	landed, err := a.Spawn(SpawnReq{Title: "landed", Claims: []string{"one/**"}})
	must(t, err)
	killed, err := a.Spawn(SpawnReq{Title: "killed", Claims: []string{"two/**"}})
	must(t, err)
	for _, t2 := range []string{landed.Worktree, killed.Worktree} {
		write(t, t2, "wip.txt", "wip\n")
	}
	a.NewCheckpointWatcher().Tick()
	for _, id := range []string{landed.ID, killed.ID} {
		if _, err := checkpoint.Lookup(a.Root, id); err != nil {
			t.Fatalf("%s: no checkpoint: %v", id, err)
		}
	}

	commitAll(t, landed.Worktree, "one")
	must(t, a.Done(landed.ID, "one"))
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TrainOK {
		t.Fatalf("land: %+v", rs)
	}
	if _, err := checkpoint.Lookup(a.Root, landed.ID); !errors.Is(err, checkpoint.ErrNone) {
		t.Fatalf("landed task kept its checkpoint: %v", err)
	}

	must(t, a.Kill(killed.ID, true))
	if _, err := checkpoint.Lookup(a.Root, killed.ID); !errors.Is(err, checkpoint.ErrNone) {
		t.Fatalf("killed task kept its checkpoint: %v", err)
	}
	if _, err := gitx.RevParse(a.Root, wipRef(killed.ID)); err != nil {
		t.Fatalf("kill lost the uncommitted work: %v", err)
	}
}
