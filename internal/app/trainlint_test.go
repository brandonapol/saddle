package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/refguard"
	"github.com/brandonapol/saddle/internal/store"
)

// #395: the train's lint gate waits its turn on the repo hooks' lock, as
// done's does, so it never runs beside another worktree's check.
func TestTrainLintWaitsOnTheHookLock(t *testing.T) {
	a := trainSetup(t)
	cmd, runs := countingGate(t)
	a.Cfg.Train.Lint = config.Lint{Cmd: cmd, Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "lint"})
	must(t, err)
	f, err := os.OpenFile(filepath.Join(hookState(t, tk.Worktree), "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	must(t, err)
	defer f.Close()
	must(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX))
	type result struct {
		msg string
		env *GateEnvProblem
		err error
	}
	done := make(chan result, 1)
	go func() {
		msg, env, err := a.trainLint(tk.ID, tk.Worktree)
		done <- result{msg, env, err}
	}()
	select {
	case r := <-done:
		t.Fatalf("the train's lint gate ran while a hook held the lock (%+v)", r)
	case <-time.After(500 * time.Millisecond):
	}
	if n := runs(); n != 0 {
		t.Fatalf("gate ran %d times under another's lock", n)
	}
	must(t, syscall.Flock(int(f.Fd()), syscall.LOCK_UN))
	select {
	case r := <-done:
		if r.msg != "" || r.env != nil || r.err != nil {
			t.Fatalf("trainLint = %+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the train's lint gate never ran after the lock was free")
	}
	if n := runs(); n != 1 {
		t.Fatalf("gate ran %d times", n)
	}
}

// #395, the race #271 fixed for done: an agent's commit runs the repo's
// pre-commit hook (saddle's wrapper, under its flock) while the train lints
// a landing. The train waits for the hook instead of running beside it and
// refusing the branch for the overlap.
func TestTrainLintWaitsForTheRepoHook(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	a := trainSetup(t)
	cmd, runs, overlaps := exclusiveGate(t)
	write(t, a.Root, "Makefile", "lint:\n\t@"+cmd+"\n")
	write(t, a.Root, "git/hooks/pre-commit", "#!/bin/sh\nmake lint\n")
	must(t, os.Chmod(filepath.Join(a.Root, "git/hooks/pre-commit"), 0o755))
	commitAll(t, a.Root, "repo gate")
	if _, err := refguard.InstallRepoHooks(a.Root); err != nil {
		t.Fatal(err)
	}
	if g := a.LintGate(); g.Cmd != "make lint" {
		t.Fatalf("gate = %+v", g)
	}
	committer, err := a.Spawn(SpawnReq{Title: "committer"})
	must(t, err)
	lander, err := a.Spawn(SpawnReq{Title: "lander"})
	must(t, err)
	write(t, lander.Worktree, "lander.txt", "lander\n")
	git(t, lander.Worktree, "add", "-A")
	git(t, lander.Worktree, "-c", "core.hooksPath=/dev/null", "commit", "-qm", "lander")
	write(t, committer.Worktree, "committer.txt", "committer\n")
	git(t, committer.Worktree, "add", "-A")

	var wg sync.WaitGroup
	var commitOut []byte
	var commitErr, lintErr error
	var msg string
	var env *GateEnvProblem
	wg.Add(2)
	go func() {
		defer wg.Done()
		c := exec.Command("git", "-C", committer.Worktree, "commit", "-qm", "committer")
		c.Env = append(os.Environ(), "SADDLE_TASK="+committer.ID, "SADDLE_TRAIN=", "SADDLE_FAST_HOOK=")
		commitOut, commitErr = c.CombinedOutput()
	}()
	go func() {
		defer wg.Done()
		msg, env, lintErr = a.trainLint(lander.ID, lander.Worktree)
	}()
	wg.Wait()
	if commitErr != nil {
		t.Fatalf("the committer's hook failed: %v\n%s", commitErr, commitOut)
	}
	if msg != "" || env != nil || lintErr != nil {
		t.Fatalf("the train refused the lander: msg=%q env=%+v err=%v", msg, env, lintErr)
	}
	if n := overlaps(); n != 0 {
		t.Fatalf("the train's lint gate ran beside the repo hook %d times", n)
	}
	if n := runs(); n != 2 {
		t.Fatalf("gate ran %d times, want the hook once and the train once", n)
	}
	// The tree the hook just passed isn't linted again by the train.
	if msg, env, err := a.trainLint(committer.ID, committer.Worktree); msg != "" || env != nil || err != nil {
		t.Fatalf("trainLint on the committer: %q %+v %v", msg, env, err)
	}
	if n := runs(); n != 2 {
		t.Fatalf("the train re-ran the gate on the tree the hook passed (%d runs)", n)
	}
}

// #395: golangci-lint refusing to run beside another is the machine, not
// the branch: the train retries instead of sending the branch back.
func TestLandRetriesParallelLint(t *testing.T) {
	a := trainSetup(t)
	tk := queueTask(t, a, "t1", "lint", map[string]string{"x.txt": "x\n"})
	once := filepath.Join(t.TempDir(), "once")
	a.Cfg.Train.Lint = config.Lint{Cmd: "if [ ! -e " + shellQuote(once) + " ]; then touch " + shellQuote(once) + "; echo 'Error: parallel golangci-lint is running'; exit 3; fi", Set: true}
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TrainOK {
		t.Fatalf("results = %+v", rs)
	}
	if got, _ := a.Store.Task(tk.ID); got.Status != store.Landed {
		t.Fatalf("status = %s", got.Status)
	}
}

// #395: when the machine keeps failing the lint gate, the branch stays
// queued and uncharged rather than being told to fix it.
func TestLandKeepsBranchQueuedWhenLintKeepsFailingOnTheMachine(t *testing.T) {
	a := trainSetup(t)
	tk := queueTask(t, a, "t1", "lint", map[string]string{"x.txt": "x\n"})
	a.Cfg.Train.Lint = config.Lint{Cmd: "echo 'Error: parallel golangci-lint is running'; exit 3", Set: true}
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TrainError {
		t.Fatalf("results = %+v", rs)
	}
	got, _ := a.Store.Task(tk.ID)
	if got.Status == store.Conflict {
		t.Fatalf("the branch was sent back for the machine's failure: %+v", got)
	}
	ns, err := a.Store.TakeNotices(tk.ID, false)
	must(t, err)
	for _, n := range ns {
		if n.Kind == store.NoticeAction {
			t.Fatalf("the producer was told to fix the machine: %+v", n)
		}
	}
}
