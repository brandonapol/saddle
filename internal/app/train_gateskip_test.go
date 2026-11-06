package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
)

// gatedTrain is a train whose test and lint gates are the same counting
// command, as in quark and saddle (`make check` for both).
func gatedTrain(t *testing.T) (a *App, runs func() int) {
	t.Helper()
	a = trainSetup(t)
	cmd, runs := countingGate(t)
	a.Cfg.Test.Cmd = cmd
	a.Cfg.Train.Lint = config.Lint{Cmd: cmd, Set: true}
	return a, runs
}

// #800: done's gate passed the branch's tree on top of the current
// integration; the rebase leaves that same tree on that same base, so the
// train lands it without running the gate a second time.
func TestLandSkipsTheGateOnATreeDoneAlreadyGated(t *testing.T) {
	a, runs := gatedTrain(t)
	tk, err := a.Spawn(SpawnReq{Title: "gated"})
	must(t, err)
	write(t, tk.Worktree, "gated.txt", "gated\n")
	commitAll(t, tk.Worktree, "gated")
	must(t, a.Done(tk.ID, "gated"))
	if n := runs(); n != 1 {
		t.Fatalf("done ran the gate %d times, want 1", n)
	}
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TrainOK {
		t.Fatalf("land = %+v", rs)
	}
	if n := runs(); n != 1 {
		t.Fatalf("the train re-ran the gate on the tree done passed (%d runs)", n)
	}
	if !hasEvent(t, a, "test_skipped", "") {
		t.Fatal("no test_skipped event")
	}
}

// #800: when the rebase changes the tree (another branch landed first), the
// train runs the gate on the new tree even though done passed the old one.
func TestLandRunsTheGateWhenTheRebasedTreeDiffers(t *testing.T) {
	a, runs := gatedTrain(t)
	var ts []store.Task
	for _, title := range []string{"first", "second"} {
		tk, err := a.Spawn(SpawnReq{Title: title})
		must(t, err)
		write(t, tk.Worktree, title+".txt", title+"\n")
		commitAll(t, tk.Worktree, title)
		must(t, a.Done(tk.ID, title))
		ts = append(ts, tk)
	}
	if n := runs(); n != 2 {
		t.Fatalf("done ran the gate %d times, want 2", n)
	}
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 2 || rs[0].State != store.TrainOK || rs[1].State != store.TrainOK {
		t.Fatalf("land = %+v", rs)
	}
	// The first lands on the integration done gated it on: skipped. The
	// second rebases onto the first's landing: a new tree, gated again.
	if n := runs(); n != 3 {
		t.Fatalf("gate ran %d times in all, want 3 (the second's rebased tree once)", n)
	}
	es, err := a.Store.Events(-1)
	must(t, err)
	skipped := map[string]bool{}
	for _, e := range es {
		if e.Kind == "test_skipped" {
			skipped[e.Task] = true
		}
	}
	if !skipped[ts[0].ID] || skipped[ts[1].ID] {
		t.Fatalf("test_skipped for %v, want only %s", skipped, ts[0].ID)
	}
}

// #800: a stamp vouches for a tree, but tests can read history. A branch
// whose tree matches a gated one on a different base isn't skipped.
func TestGateSkipNeedsTheSameBase(t *testing.T) {
	a, runs := gatedTrain(t)
	tk, err := a.Spawn(SpawnReq{Title: "based"})
	must(t, err)
	write(t, tk.Worktree, "based.txt", "based\n")
	commitAll(t, tk.Worktree, "based")
	must(t, a.lintDone(tk))
	if n := runs(); n != 1 {
		t.Fatalf("runs = %d", n)
	}
	head := git(t, tk.Worktree, "rev-parse", "HEAD")
	base := git(t, a.Root, "rev-parse", a.Cfg.Integration)
	if !a.gatedBefore(tk.Worktree, base, head, a.Cfg.Test.Cmd) {
		t.Fatal("not skipped on the same tree and base")
	}
	// Integration moves under the branch with an empty commit: the tree is
	// the same after a rebase, but the base is not the one done gated on.
	moved := git(t, a.Root, "commit-tree", base+"^{tree}", "-p", base, "-m", "moved")
	if a.gatedBefore(tk.Worktree, moved, head, a.Cfg.Test.Cmd) {
		t.Fatal("skipped on a base done never gated on")
	}
	if a.gatedBefore(tk.Worktree, base, head, "something else") {
		t.Fatal("skipped a command no gate ran")
	}
}

// #800: the stamp file kept only 200 trees, so a busy repo forgot passes the
// train could have skipped.
func TestStampLintKeepsManyTrees(t *testing.T) {
	state := t.TempDir()
	g := testGate("make check")
	first := strings.Repeat("a", 40)
	stampLint(state, first, g)
	for i := range 500 {
		stampLint(state, fmt.Sprintf("%040d", i), g)
	}
	if !passedLint(state, first, g) {
		t.Fatal("the stamp file forgot a tree 500 stamps back")
	}
}

// forgetPasses drops every pass stamp the repo's gates wrote, so the train
// has to run its gate rather than skip a tree done already passed (#800).
func forgetPasses(t *testing.T, dir string) {
	t.Helper()
	state := hookState(t, dir)
	for _, f := range []string{lintStamp, "pre-commit.ok"} {
		if err := os.Remove(filepath.Join(state, f)); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}
