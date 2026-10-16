package app

import (
	"os"
	"strings"
	"testing"
	"time"
)

func needProc(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/proc/self/cwd"); err != nil {
		t.Skip("no /proc")
	}
}

// #676: a killed task leaves no process under its run dir and no run dir.
func TestKillReapsAndRemovesRunDir(t *testing.T) {
	needProc(t)
	a, _ := setup(t)
	task, err := a.Spawn(SpawnReq{Title: "reap"})
	must(t, err)
	run := a.stateDir("run", task.ID)
	must(t, os.MkdirAll(run, 0o755))
	held := daemon(t, run)
	other := daemon(t, t.TempDir())

	must(t, a.Kill(task.ID, true))
	if held.Wait() == nil {
		t.Error("process in the run dir exited cleanly; it should have been killed")
	}
	if !pidAlive(other.Process.Pid) {
		t.Error("a process outside the run dir was killed")
	}
	if exists(run) {
		t.Error("killed task's run dir left behind")
	}
	if !exists(task.Worktree) {
		t.Error("kill --keep lost the worktree")
	}
}

// #676: a run dir something still holds (a process that survives SIGKILL
// can't be made in a test, so the dir is held through a skipped reap) is
// kept with a note.
func TestRemoveRunDirKeepsHeldDir(t *testing.T) {
	needProc(t)
	a, _ := setup(t)
	run := a.stateDir("run", "t77")
	must(t, os.MkdirAll(run, 0o755))
	held := daemon(t, run)
	a.runReap = func(string) {} // the holder survives the reap

	a.cleanRunDir("t77")
	if !exists(run) {
		t.Fatal("a held run dir was removed")
	}
	if !pidAlive(held.Process.Pid) {
		t.Fatal("holder died")
	}
	evs, err := a.Store.Events(50)
	must(t, err)
	found := false
	for _, e := range evs {
		found = found || (e.Task == "t77" && e.Kind == "run_dir_kept" && strings.Contains(e.Data, run))
	}
	if !found {
		t.Fatalf("no run_dir_kept note in %+v", evs)
	}
}

// #676: layout-* worktrees from runs that died midway go, with their git
// registration; a fresh one, or one a live process works in, stays.
func TestSweepStaleLayouts(t *testing.T) {
	needProc(t)
	a, _ := setup(t)
	mk := func(name string) string {
		d := a.stateDir(name)
		git(t, a.Root, "worktree", "add", "-q", "--detach", d, "HEAD")
		return d
	}
	stale, fresh, busy := mk("layout-1"), mk("layout-2"), mk("layout-3")
	old := time.Now().Add(-2 * time.Hour)
	for _, d := range []string{stale, busy} {
		must(t, os.Chtimes(d, old, old))
	}
	holder := daemon(t, busy)

	res := a.SweepStaleLayouts(time.Hour)
	if len(res.Dirs) != 1 || res.Dirs[0] != stale {
		t.Fatalf("sweep = %+v, want only %s", res, stale)
	}
	if exists(stale) || !exists(fresh) || !exists(busy) {
		t.Fatalf("stale=%v fresh=%v busy=%v", exists(stale), exists(fresh), exists(busy))
	}
	if strings.Contains(git(t, a.Root, "worktree", "list"), "layout-1") {
		t.Error("stale layout still registered with git")
	}
	if !pidAlive(holder.Process.Pid) {
		t.Error("holder in a busy layout dir was killed")
	}
}
