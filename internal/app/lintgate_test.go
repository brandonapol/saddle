package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// badLint fails, printing the offending files, when any tracked-tree file
// holds LINTBAD: a stand-in for gofmt -l.
const badLint = `bad=$(grep -rl --exclude-dir=.git --exclude-dir=.saddle LINTBAD . || true); if [ -n "$bad" ]; then echo "lint: needs fixing: $bad"; exit 1; fi`

func TestLintCmdResolution(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	a := trainSetup(t)
	// Queued before the gate is set, so done's own check doesn't stop it.
	tk := queueTask(t, a, "t1", "lint", map[string]string{"x.go": "LINTBAD\n"})
	a.Cfg.Train.Lint = config.Lint{Cmd: badLint, Set: true}
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
	t.Parallel()
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
	t.Parallel()
	a := trainSetup(t)
	count := filepath.Join(t.TempDir(), "count")
	a.Cfg.Test.Cmd = "echo x >> " + count
	a.Cfg.Train.Lint = config.Lint{Cmd: a.Cfg.Test.Cmd, Set: true}
	queueTask(t, a, "t1", "once", map[string]string{"x.txt": "x\n"})
	before, _ := os.ReadFile(count) // done may have run the gate
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TrainOK {
		t.Fatalf("results = %+v", rs)
	}
	b, err := os.ReadFile(count)
	must(t, err)
	if n := strings.Count(string(b), "x") - strings.Count(string(before), "x"); n != 1 {
		t.Fatalf("the train ran it %d times", n)
	}
}

// #228: a gate make has no rule for (`make fi`) is the gate's fault, not
// the branch's. done and the train skip it, so it never counts toward
// max_attempts, and the orchestrator is told how to fix it.
func TestBrokenGateDoesNotFailTheBranch(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	a := trainSetup(t)
	write(t, a.Root, "Makefile", "check:\n\ttrue\nfix:\n\ttrue\n")
	commitAll(t, a.Root, "makefile")
	a.Cfg.Train.MaxAttempts = 1
	a.Cfg.Train.Lint = config.Lint{Cmd: "make fi", Set: true}
	tk := queueTask(t, a, "t1", "broken gate", map[string]string{"x.txt": "x\n"})
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TrainOK {
		t.Fatalf("results = %+v", rs)
	}
	if n := a.attempts(tk.ID); n != 0 {
		t.Fatalf("attempts = %d", n)
	}
	ns, err := a.Store.TakeNotices(OrchestratorID, false)
	must(t, err)
	found := false
	for _, n := range ns {
		found = found || (strings.Contains(n.Text, "`make fi`") && strings.Contains(n.Text, "lint.cmd"))
	}
	if !found {
		t.Fatalf("orchestrator notices = %+v", ns)
	}

	// A gate that is red for a real reason still refuses.
	a.Cfg.Train.Lint = config.Lint{Cmd: "make check && false", Set: true}
	tk2, err := a.Spawn(SpawnReq{Title: "red"})
	must(t, err)
	if err := a.lintDone(tk2); err == nil {
		t.Fatal("a red gate passed")
	}
}

// #269: done's lint gate times out like the train's, killing what it
// started, and refuses with the output tail.
func TestLintDoneTimesOut(t *testing.T) {
	a := trainSetup(t)
	setGateTimeout(t, "300ms")
	pidf := filepath.Join(t.TempDir(), "child")
	a.Cfg.Train.Lint = config.Lint{Cmd: "sleep 600 & echo $! > " + pidf + "; echo linting; wait", Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "lint"})
	must(t, err)
	write(t, tk.Worktree, "x.go", "x\n")
	commitAll(t, tk.Worktree, "x")
	err = a.lintDone(tk)
	if err == nil || !strings.Contains(err.Error(), "linting") || !strings.Contains(err.Error(), "gate timed out after 300ms") {
		t.Fatalf("done on a hung gate: %v", err)
	}
	gone(t, pidf)
}

// countingGate is a gate that logs each run to a file and passes.
func countingGate(t *testing.T) (cmd string, runs func() int) {
	log := filepath.Join(t.TempDir(), "runs")
	return "echo run >> " + shellQuote(log), func() int {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), "run")
	}
}

func hookState(t *testing.T, dir string) string {
	t.Helper()
	common, err := gitx.Run(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	must(t, err)
	state := filepath.Join(common, "saddle-hooks")
	must(t, os.MkdirAll(state, 0o755))
	return state
}

// #271: done doesn't check again a tree its gate already passed.
func TestLintDoneSkipsATreeItPassed(t *testing.T) {
	a := trainSetup(t)
	cmd, runs := countingGate(t)
	a.Cfg.Train.Lint = config.Lint{Cmd: cmd, Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "lint"})
	must(t, err)
	write(t, tk.Worktree, "x.go", "x\n")
	commitAll(t, tk.Worktree, "x")
	must(t, a.lintDone(tk))
	must(t, a.lintDone(tk))
	if n := runs(); n != 1 {
		t.Fatalf("gate ran %d times on one tree", n)
	}
	write(t, tk.Worktree, "x.go", "y\n") // uncommitted: not the tree that passed
	must(t, a.lintDone(tk))
	if n := runs(); n != 2 {
		t.Fatalf("gate ran %d times; a dirty tree must be checked", n)
	}
}

// #271: done doesn't re-run the repo's gate on the tree the repo's
// pre-commit hook (saddle's wrapper) passed moments before.
func TestLintDoneHonoursTheHookStamp(t *testing.T) {
	a := trainSetup(t)
	cmd, runs := countingGate(t)
	write(t, a.Root, "Makefile", "lint:\n\t"+cmd+"\n")
	commitAll(t, a.Root, "makefile")
	tk, err := a.Spawn(SpawnReq{Title: "lint"})
	must(t, err)
	write(t, tk.Worktree, "x.go", "x\n")
	commitAll(t, tk.Worktree, "x")
	if g := a.LintGate(); g.Cmd != "make lint" {
		t.Fatalf("gate = %+v", g)
	}
	tree, err := gitx.Run(tk.Worktree, "write-tree")
	must(t, err)
	must(t, os.WriteFile(filepath.Join(hookState(t, tk.Worktree), "pre-commit.ok"), []byte("0000\n"+tree+"\n"), 0o644))
	must(t, a.lintDone(tk))
	if n := runs(); n != 0 {
		t.Fatalf("gate ran %d times on a tree the hook passed", n)
	}
	// A configured gate isn't the hook: its stamp proves nothing.
	a.Cfg.Train.Lint = config.Lint{Cmd: cmd + " # configured", Set: true}
	must(t, a.lintDone(tk))
	if n := runs(); n != 1 {
		t.Fatalf("configured gate ran %d times", n)
	}
}

// #271: done's gate waits its turn on the repo hooks' lock, so it never
// runs alongside another worktree's make check.
func TestLintDoneWaitsOnTheHookLock(t *testing.T) {
	a := trainSetup(t)
	cmd, runs := countingGate(t)
	a.Cfg.Train.Lint = config.Lint{Cmd: cmd, Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "lint"})
	must(t, err)
	f, err := os.OpenFile(filepath.Join(hookState(t, tk.Worktree), "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	must(t, err)
	defer f.Close()
	must(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX))
	done := make(chan error, 1)
	go func() { done <- a.lintDone(tk) }()
	select {
	case err := <-done:
		t.Fatalf("done's gate ran while a hook held the lock (%v)", err)
	case <-time.After(500 * time.Millisecond):
	}
	if n := runs(); n != 0 {
		t.Fatalf("gate ran %d times under another's lock", n)
	}
	must(t, syscall.Flock(int(f.Fd()), syscall.LOCK_UN))
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("done's gate never ran after the lock was free")
	}
}

// #271: golangci-lint refusing to run beside another is the environment,
// not the tree: done retries instead of telling the agent to fix it.
func TestLintDoneRetriesParallelLint(t *testing.T) {
	a := trainSetup(t)
	once := filepath.Join(t.TempDir(), "once")
	a.Cfg.Train.Lint = config.Lint{Cmd: "if [ ! -e " + shellQuote(once) + " ]; then touch " + shellQuote(once) + "; echo 'Error: parallel golangci-lint is running'; exit 3; fi", Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "lint"})
	must(t, err)
	must(t, a.lintDone(tk))
}

// #270: the repo's hook runs make through a variable ("${MAKE}" check, as
// quark's does) and [test] cmd is make check. The gate is the same check, so
// a landing runs it once, and the pre-publish gate lists it once.
func TestLandRunsMakeVariableHookOnceWhenSameAsTest(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	a := trainSetup(t)
	count := filepath.Join(t.TempDir(), "count")
	write(t, a.Root, "Makefile", "check:\n\techo x >> "+count+"\n")
	write(t, a.Root, "git/hooks/pre-commit", "#!/bin/sh\nMAKE=make\n\"${MAKE}\" check\n")
	commitAll(t, a.Root, "repo gate")
	a.Cfg.Test.Cmd = "make  check"
	if g := a.LintGate(); g.Cmd != "make check" {
		t.Fatalf("gate = %+v", g)
	}
	if cs := a.GateChecks(false); len(cs) != 1 {
		t.Fatalf("pre-publish checks = %+v", cs)
	}
	queueTask(t, a, "t1", "once", map[string]string{"x.txt": "x\n"})
	before, _ := os.ReadFile(count)
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TrainOK {
		t.Fatalf("results = %+v", rs)
	}
	b, err := os.ReadFile(count)
	must(t, err)
	if n := strings.Count(string(b), "x") - strings.Count(string(before), "x"); n != 1 {
		t.Fatalf("the train ran make check %d times", n)
	}
}

// done's gate runs with TMPDIR and GOTMPDIR in a per-run dir under
// GateTmpdir, like the train's (#320): with the inherited TMPDIR full (here
// read-only), done still passes and writes nothing there.
func TestLintDoneHonoursGateTmpdirWithPoisonedTMPDIR(t *testing.T) {
	a := trainSetup(t)
	a.Cfg.Train.Tmpdir = filepath.Join(t.TempDir(), "gates")
	poison := filepath.Join(t.TempDir(), "full")
	must(t, os.Mkdir(poison, 0o555))
	t.Cleanup(func() { _ = os.Chmod(poison, 0o755) })
	t.Setenv("TMPDIR", poison)
	seen := filepath.Join(t.TempDir(), "seen")
	a.Cfg.Train.Lint = config.Lint{Cmd: `mktemp >/dev/null && echo "$TMPDIR $GOTMPDIR" > ` + seen, Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "tmp"})
	must(t, err)
	write(t, tk.Worktree, "x.go", "ok\n")
	commitAll(t, tk.Worktree, "x")
	must(t, a.lintDone(tk))
	b, err := os.ReadFile(seen)
	must(t, err)
	dirs := strings.Fields(string(b))
	if len(dirs) != 2 || !strings.HasPrefix(dirs[0], a.GateTmpdir()+string(filepath.Separator)) || dirs[1] != dirs[0] {
		t.Fatalf("gate saw TMPDIR GOTMPDIR %q, want a per-run dir under %s", b, a.GateTmpdir())
	}
	if es, _ := os.ReadDir(poison); len(es) != 0 {
		t.Fatalf("done's gate wrote %d entries to the inherited TMPDIR", len(es))
	}
	if _, err := os.Stat(dirs[0]); !os.IsNotExist(err) {
		t.Fatalf("the per-run dir %s must be removed after", dirs[0])
	}
}

// A done gate that fails on temp space names the temp entries with the
// most files (#320), so a leak of empty dirs shows at once.
func TestLintDoneTempFailureNamesTheBiggestLeak(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	dir := withScratchDir(t, a)
	leak := filepath.Join(dir, "saddle-app-runq-leak")
	for i := range 30 {
		must(t, os.MkdirAll(filepath.Join(leak, strings.Repeat("d", i+1)), 0o755))
	}
	a.Cfg.Train.Lint = config.Lint{Cmd: `echo "mkdir: disk quota exceeded"; exit 1`, Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "quota"})
	must(t, err)
	write(t, tk.Worktree, "x.go", "x\n")
	commitAll(t, tk.Worktree, "x")
	err = a.lintDone(tk)
	if err == nil || !strings.Contains(err.Error(), "Most files: "+leak) {
		t.Fatalf("env failure must name the leak: %v", err)
	}
}
