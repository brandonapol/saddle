package app

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/gc"
	"github.com/brandonapol/saddle/internal/gitx"
)

// #802: the engine's gc removes a killed task's dirty worktree once its
// uncommitted work is in refs/saddle/wip/<task>; a live task is untouched.
func TestAutoGCRemovesKilledDirtyWorktreeKeepsLive(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	dead, err := a.Spawn(SpawnReq{Title: "dead"})
	must(t, err)
	must(t, a.Kill(dead.ID, true))
	write(t, dead.Worktree, "wip.txt", "wip\n")
	live, err := a.Spawn(SpawnReq{Title: "live"})
	must(t, err)
	write(t, live.Worktree, "mine.txt", "mine\n")
	git(t, a.Root, "update-ref", wipRef(live.ID), "HEAD")

	r, err := a.AutoGC(time.Now())
	must(t, err)
	if r.Skipped != "" {
		t.Fatalf("skipped: %s", r.Skipped)
	}
	if exists(dead.Worktree) {
		t.Fatal("auto gc kept a killed task's dirty worktree")
	}
	if out := git(t, a.Root, "show", wipRef(dead.ID)+":wip.txt"); out != "wip" {
		t.Fatalf("snapshot wip.txt = %q", out)
	}
	if !exists(live.Worktree+"/mine.txt") || !gitx.BranchExists(a.Root, live.Branch) {
		t.Fatal("auto gc touched a live task's worktree or branch")
	}
	if _, err := gitx.RevParse(a.Root, wipRef(live.ID)); err != nil {
		t.Fatal("auto gc removed a live task's wip ref")
	}
	if !hasEvent(t, a, EventGCAuto, "removed") {
		t.Fatal("no gc_auto event")
	}
}

// #802: a leftover whose task the store doesn't know (a spawn racing the
// tick, or a reset state.db) is left to a manual saddle gc, and so is
// anything of a task that is live when its turn comes.
func TestAutoGCKeepsUnknownAndLiveTasks(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	live, err := a.Spawn(SpawnReq{Title: "live"})
	must(t, err)
	orphan := a.stateDir("worktrees", "t99-orphan")
	git(t, a.Root, "worktree", "add", "-q", "-b", "saddle/t99-orphan", orphan, a.Cfg.Integration)

	ls, err := a.Leftovers()
	must(t, err)
	// The live task's things as if listed before it spawned.
	ls = append(ls, Leftover{Kind: "worktree", Name: live.Worktree}, Leftover{Kind: "branch", Name: live.Branch},
		Leftover{Kind: "ref", Name: wipRef(live.ID)})
	out, _ := a.removeLeftovers(ls, a.autoKeep)
	for _, l := range out {
		if l.Keep == "" {
			t.Errorf("auto gc removed %s %s", l.Kind, l.Name)
		}
	}
	if !exists(orphan) || !exists(live.Worktree) || !gitx.BranchExists(a.Root, live.Branch) {
		t.Fatal("auto gc removed a worktree or branch it should have left")
	}
}

// #802: the tick waits for the train: it skips while train.lock is held.
func TestAutoGCSkipsWhileTrainBusy(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	dead, err := a.Spawn(SpawnReq{Title: "dead"})
	must(t, err)
	must(t, a.Kill(dead.ID, true))
	unlock, ok, err := a.TryLockTrain()
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	r, err := a.AutoGC(time.Now())
	unlock()
	must(t, err)
	if r.Skipped == "" || !exists(dead.Worktree) {
		t.Fatalf("report %+v, worktree exists %v: want skipped", r, exists(dead.Worktree))
	}
}

// #802: backups beyond the newest few, old noisy events and stale ci-fix
// scratch go; landed events stay.
func TestAutoGCPrunesBackupsRowsAndScratch(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	for i := range gc.BackupKeep + 3 {
		git(t, a.Root, "update-ref", fmt.Sprintf("%s20261101T%06dZ-abc", gc.BackupPrefix, i), "HEAD")
	}
	a.Store.Event("t1", EventCIError, "gh: 502")
	a.Store.Event("t1", "landed", "abc")
	fix := a.stateDir("ci-fix")
	git(t, a.Root, "worktree", "add", "-q", "--detach", fix, "HEAD")
	old := time.Now().Add(-2 * GateSweepAge)
	must(t, os.Chtimes(fix, old, old))

	r, err := a.AutoGC(time.Now().Add(8 * 24 * time.Hour))
	must(t, err)
	if r.Backups != 3 {
		t.Fatalf("pruned %d backups, want 3", r.Backups)
	}
	if n := len(strings.Fields(git(t, a.Root, "for-each-ref", "--format=%(refname)", gc.BackupPrefix))); n != gc.BackupKeep {
		t.Fatalf("%d backups left, want %d", n, gc.BackupKeep)
	}
	if hasEvent(t, a, EventCIError, "gh: 502") || !hasEvent(t, a, "landed", "abc") {
		t.Fatal("want the old ci_error pruned and landed kept")
	}
	if exists(fix) {
		t.Fatal("auto gc kept a stale ci-fix worktree")
	}
}

// #802: RunGC ticks right away when told to, and stops with its context.
func TestRunGCTicksAndStops(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.runGC(ctx, 0, time.Hour); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for !hasEvent(t, a, EventGCAuto, "") {
		if time.Now().After(deadline) {
			t.Fatal("no gc_auto event from RunGC")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunGC didn't stop")
	}
}
