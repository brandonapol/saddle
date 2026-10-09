package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
)

// regenSetup gives the repo a derived file, gen.txt, built from a.txt and
// b.txt by the configured regen command.
func regenSetup(t *testing.T) *App {
	t.Helper()
	a := trainSetup(t)
	write(t, a.Root, "a.txt", "a\n")
	write(t, a.Root, "b.txt", "b\n")
	write(t, a.Root, "gen.txt", "a\nb\n")
	commitAll(t, a.Root, "derived file")
	a.Cfg.Regen = []config.Regen{{Paths: []string{"gen.txt"}, Cmd: "cat a.txt b.txt > gen.txt"}}
	return a
}

// Two tasks change different inputs and both regenerate gen.txt, so its text
// conflicts. The train takes integration's copy, reruns the regen command and
// commits, instead of returning the conflict.
func TestLandRegeneratesDerivedFileOnConflict(t *testing.T) {
	t.Parallel()
	a := regenSetup(t)
	t1, _ := a.Spawn(SpawnReq{Title: "one"})
	t2, _ := a.Spawn(SpawnReq{Title: "two"})
	write(t, t1.Worktree, "a.txt", "A\n")
	write(t, t1.Worktree, "gen.txt", "A\nb\n")
	commitAll(t, t1.Worktree, "one")
	write(t, t2.Worktree, "b.txt", "B\n")
	write(t, t2.Worktree, "gen.txt", "a\nB\n")
	commitAll(t, t2.Worktree, "two")
	must(t, a.Done(t1.ID, "one"))
	must(t, a.Done(t2.ID, "two"))
	rs, err := a.Land()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].State != store.TrainOK || rs[1].State != store.TrainOK {
		t.Fatalf("results = %+v", rs)
	}
	if got := git(t, a.Root, "show", a.Cfg.Integration+":gen.txt"); got != "A\nB" {
		t.Fatalf("gen.txt on integration = %q", got)
	}
	if !strings.Contains(rs[1].Note, "regenerated gen.txt") {
		t.Fatalf("note = %q", rs[1].Note)
	}
}

// A conflict outside the regen globs still goes back to the producer, even
// when a regen file conflicts too.
func TestRegenLeavesOtherConflictsToProducer(t *testing.T) {
	t.Parallel()
	a := regenSetup(t)
	t1, _ := a.Spawn(SpawnReq{Title: "one"})
	t2, _ := a.Spawn(SpawnReq{Title: "two"})
	write(t, t1.Worktree, "README.md", "one\n")
	write(t, t1.Worktree, "gen.txt", "A\nb\n")
	commitAll(t, t1.Worktree, "one")
	write(t, t2.Worktree, "README.md", "two\n")
	write(t, t2.Worktree, "gen.txt", "a\nB\n")
	commitAll(t, t2.Worktree, "two")
	must(t, a.Done(t1.ID, "one"))
	must(t, a.Done(t2.ID, "two"))
	rs, err := a.Land()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[1].State != store.TrainError || !strings.Contains(rs[1].Note, "README.md") {
		t.Fatalf("results = %+v", rs)
	}
}

// A failing regen command returns the branch to its producer with the output.
func TestRegenFailureReturnsToProducer(t *testing.T) {
	t.Parallel()
	a := regenSetup(t)
	a.Cfg.Regen[0].Cmd = "echo boom >&2; exit 3"
	t1, _ := a.Spawn(SpawnReq{Title: "one"})
	t2, _ := a.Spawn(SpawnReq{Title: "two"})
	write(t, t1.Worktree, "gen.txt", "A\nb\n")
	commitAll(t, t1.Worktree, "one")
	write(t, t2.Worktree, "gen.txt", "a\nB\n")
	commitAll(t, t2.Worktree, "two")
	must(t, a.Done(t1.ID, "one"))
	must(t, a.Done(t2.ID, "two"))
	rs, err := a.Land()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[1].State != store.TrainError {
		t.Fatalf("results = %+v", rs)
	}
	ns, _ := a.Store.TakeNotices(t2.ID, true)
	if len(ns) == 0 || !strings.Contains(ns[len(ns)-1].Text, "boom") {
		t.Fatalf("notice = %+v", ns)
	}
}
