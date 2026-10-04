package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
)

// badLint fails, printing the offending files, when any tracked-tree file
// holds LINTBAD: a stand-in for gofmt -l.
const badLint = `bad=$(grep -rl --exclude-dir=.git --exclude-dir=.saddle LINTBAD . || true); if [ -n "$bad" ]; then echo "lint: needs fixing: $bad"; exit 1; fi`

func TestLintCmdResolution(t *testing.T) {
	a, _ := setup(t)
	if g := a.LintGate(); g.Cmd != "" {
		t.Fatalf("no gate in the repo: %+v", g)
	}
	write(t, a.Root, "Makefile", "lint:\n\ttrue\nfix:\n\ttrue\n")
	if g := a.LintGate(); g.Cmd != "make lint" || g.Fix != "make fix" {
		t.Fatalf("detected: %+v", g)
	}
	a.Cfg.Train.Lint = config.Lint{Cmd: "make check", Set: true}
	if g := a.LintGate(); g.Cmd != "make check" || g.Fix != "make fix" {
		t.Fatalf("configured: %+v", g)
	}
	a.Cfg.Train.Lint = config.Lint{Set: true}
	if g := a.LintGate(); g.Cmd != "" {
		t.Fatalf(`lint.cmd = "" must disable: %+v`, g)
	}
}

// done refuses a branch the repo's gate rejects, with the output, and
// accepts it once fixed (#212).
func TestLintDoneRefusesRedAndPassesGreen(t *testing.T) {
	a := trainSetup(t)
	a.Cfg.Train.Lint = config.Lint{Cmd: badLint, Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "lint"})
	must(t, err)
	write(t, tk.Worktree, "x.go", "LINTBAD\n")
	commitAll(t, tk.Worktree, "bad")
	err = a.lintDone(tk)
	if err == nil {
		t.Fatal("done accepted a branch the gate rejects")
	}
	for _, want := range []string{"lint: needs fixing: ./x.go", "--no-verify", "call the saddle done tool again"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q lacks %q", err, want)
		}
	}
	write(t, tk.Worktree, "x.go", "ok\n")
	commitAll(t, tk.Worktree, "fix")
	must(t, a.lintDone(tk))

	a.Cfg.Train.Lint = config.Lint{Set: true}
	write(t, tk.Worktree, "x.go", "LINTBAD\n")
	commitAll(t, tk.Worktree, "bad again")
	must(t, a.lintDone(tk)) // disabled
}

// The train runs lint.cmd after test.cmd on the rebased tree and returns a
// red gate to the producer like red tests, with the output tail.
func TestLandReturnsLintFailureToProducer(t *testing.T) {
	a := trainSetup(t)
	a.Cfg.Train.Lint = config.Lint{Cmd: badLint, Set: true}
	tk := queueTask(t, a, "t1", "lint", map[string]string{"x.go": "LINTBAD\n"})
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TestFailed || rs[0].Note != "lint failed" {
		t.Fatalf("results = %+v", rs)
	}
	ns, err := a.Store.TakeNotices(tk.ID, false)
	must(t, err)
	if len(ns) == 0 || !strings.Contains(ns[len(ns)-1].Text, "lint: needs fixing: ./x.go") ||
		!strings.Contains(ns[len(ns)-1].Text, "--no-verify") {
		t.Fatalf("producer notices = %+v", ns)
	}
	if got, _ := a.Store.Task(tk.ID); got.Status != store.Conflict {
		t.Fatalf("status = %s", got.Status)
	}

	write(t, tk.Worktree, "x.go", "ok\n")
	commitAll(t, tk.Worktree, "fix")
	must(t, a.Store.Enqueue(tk.ID))
	rs, err = a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TrainOK {
		t.Fatalf("after the fix: %+v", rs)
	}
}

// A regen commit the train makes is gated too: lint runs on the tree the
// train is about to land, not just the producer's commits.
func TestLandLintsAfterRegen(t *testing.T) {
	a := regenSetup(t)
	a.Cfg.Train.Lint = config.Lint{Cmd: badLint, Set: true}
	a.Cfg.Regen[0].Cmd = "cat a.txt b.txt > gen.txt; echo LINTBAD >> gen.txt"
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
	must(t, err)
	if len(rs) != 2 || rs[0].State != store.TrainOK || rs[1].State != store.TestFailed || rs[1].Note != "lint failed" {
		t.Fatalf("results = %+v", rs)
	}
}

// When lint.cmd is the test command, the train runs it once.
func TestLandRunsLintOnceWhenSameAsTest(t *testing.T) {
	a := trainSetup(t)
	count := filepath.Join(t.TempDir(), "count")
	a.Cfg.Test.Cmd = "echo x >> " + count
	a.Cfg.Train.Lint = config.Lint{Cmd: a.Cfg.Test.Cmd, Set: true}
	landTask(t, a, "t1", "once", map[string]string{"x.txt": "x\n"})
	b, err := os.ReadFile(count)
	must(t, err)
	if n := strings.Count(string(b), "x"); n != 1 {
		t.Fatalf("ran %d times", n)
	}
}
