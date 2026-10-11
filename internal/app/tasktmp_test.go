package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/scratch"
	"github.com/brandonapol/saddle/internal/store"
)

// wantTaskTmp is where task id's TMPDIR should be under scratch root dir.
func wantTaskTmp(a *App, dir, id string) string {
	return filepath.Join(dir, scratch.RepoKey(a.Root), "task-"+id)
}

// #327: a spawned agent starts with TMPDIR, GOTMPDIR and CLAUDE_CODE_TMPDIR
// in its own dir under the scratch root, which exists before it starts.
func TestSpawnPointsTMPDIRAtTaskDir(t *testing.T) {
	t.Parallel()
	a, ft := setup(t)
	dir := withScratchDir(t, a)
	tk, err := a.Spawn(SpawnReq{Title: "tmp"})
	must(t, err)
	tk, err = a.Store.Task(tk.ID)
	must(t, err)
	want := wantTaskTmp(a, dir, tk.ID)
	if !dirExists(want) {
		t.Fatalf("task tmp dir %s not created", want)
	}
	cmd := ft.cmds[tk.Window]
	for _, k := range []string{"TMPDIR", "GOTMPDIR", "CLAUDE_CODE_TMPDIR"} {
		if !strings.Contains(cmd, " "+k+"="+shellQuote(want)) {
			t.Errorf("window command lacks %s=%s: %s", k, want, cmd)
		}
	}
}

// #327: killing a task removes its tmp dir, with or without keep.
func TestKillRemovesTaskTmp(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	dir := withScratchDir(t, a)
	for _, keep := range []bool{false, true} {
		tk, err := a.Spawn(SpawnReq{Title: "tmp"})
		must(t, err)
		p := wantTaskTmp(a, dir, tk.ID)
		write(t, p, "big/blob", "x")
		must(t, a.Kill(tk.ID, keep))
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("keep=%v: %s survived kill: %v", keep, p, err)
		}
	}
}

// #327: landing a task removes its tmp dir.
func TestLandRemovesTaskTmp(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	dir := withScratchDir(t, a)
	tk, err := a.Spawn(SpawnReq{Title: "landed", Claims: []string{"one/**"}})
	must(t, err)
	p := wantTaskTmp(a, dir, tk.ID)
	write(t, p, "blob", "x")
	write(t, tk.Worktree, "one/a.txt", "a\n")
	commitAll(t, tk.Worktree, "one")
	must(t, a.Done(tk.ID, "one"))
	if !dirExists(p) {
		t.Fatalf("done removed %s; the agent may still be sent back to fix its branch", p)
	}
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TrainOK {
		t.Fatalf("land: %+v", rs)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatalf("%s survived landing: %v", p, err)
	}
}

// #327: the sweep leaves a live task's tmp dir alone however old it is, and
// removes the dirs of tasks that are gone, killed or landed however fresh.
func TestSweepKeepsLiveTaskTmpAndRemovesOrphans(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	dir := withScratchDir(t, a)
	live, err := a.Spawn(SpawnReq{Title: "live"})
	must(t, err)
	livePath := wantTaskTmp(a, dir, live.ID)
	fill(t, livePath, 10, 3*time.Hour)

	orphan := wantTaskTmp(a, dir, "t999")
	fill(t, orphan, 10, 0)
	dead, err := a.Spawn(SpawnReq{Title: "dead"})
	must(t, err)
	must(t, a.Store.SetStatus(dead.ID, store.Killed)) // killed by hand: kill never ran
	deadPath := wantTaskTmp(a, dir, dead.ID)
	fill(t, deadPath, 10, 0)

	dry := a.SweepScratch(true, 0)
	if !dirExists(orphan) || !dirExists(deadPath) {
		t.Fatal("a dry run removed something")
	}
	if !removed(dry, orphan) || !removed(dry, deadPath) {
		t.Fatalf("dry run doesn't list the orphans: %+v", dry.Removed)
	}

	res := a.SweepScratch(false, 0)
	if !dirExists(livePath) {
		t.Fatalf("sweep removed live task's %s: %+v", livePath, res)
	}
	for _, p := range []string{orphan, deadPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("orphan %s survived the sweep: %v", p, err)
		}
	}
	if removed(res, livePath) {
		t.Fatalf("sweep reports the live dir removed: %+v", res.Removed)
	}
}

func removed(r scratch.Result, p string) bool {
	for _, e := range r.Removed {
		if e.Path == p {
			return true
		}
	}
	return false
}

// #327: the brief tells agents to keep scratch under $TMPDIR, since env
// vars can't redirect a hardcoded /tmp path.
func TestWorkerBriefTellsAgentsToUseTMPDIR(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	b := a.workerBrief(store.Task{ID: "t1", Title: "x"}, nil)
	if !strings.Contains(b, "$TMPDIR") || !strings.Contains(b, "/tmp/") {
		t.Fatalf("brief lacks the $TMPDIR rule:\n%s", b)
	}
}
