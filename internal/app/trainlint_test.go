package app

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
)

// #529: a done gate holding the shared hooks lock must not stall the train's
// lint step.
func TestTrainLintIgnoresTheHooksLock(t *testing.T) {
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
	go func() {
		msg, env, err := a.trainLint(tk.ID, tk.Worktree)
		if err == nil && (msg != "" || env != nil) {
			err = errors.New("lint not green: " + msg)
		}
		done <- err
	}()
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the train's lint gate waited on the hooks lock")
	}
	if n := runs(); n != 1 {
		t.Fatalf("gate ran %d times", n)
	}
}

// #395: two lint runs (the train's and done's) never overlap: the train's
// lint waits on the lint lock.
func TestTrainLintWaitsOnTheLintLock(t *testing.T) {
	a := trainSetup(t)
	cmd, runs := countingGate(t)
	a.Cfg.Train.Lint = config.Lint{Cmd: cmd, Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "lint"})
	must(t, err)
	f, err := os.OpenFile(filepath.Join(hookState(t, tk.Worktree), lintLockName), os.O_CREATE|os.O_RDWR, 0o644)
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
