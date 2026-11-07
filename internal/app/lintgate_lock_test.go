package app

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
)

// holdLock takes the flock on <state>/name from a separate open file, as
// another worktree's hook would, and records holder in its holder file.
func holdLock(t *testing.T, state, name, holder string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(state, name), os.O_CREATE|os.O_RDWR, 0o644)
	must(t, err)
	t.Cleanup(func() { f.Close() })
	must(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX))
	if holder != "" {
		must(t, os.WriteFile(filepath.Join(state, name+holderSuffix), []byte(holder+"\n"), 0o644))
	}
}

// #799: with both gate locks held, done's lint gate waits once, bounded by
// GateTimeout (not once per lock), and never runs the gate without the lock:
// it defers the tree to the train's gate instead, naming who held the lock.
func TestLintDoneBoundedWaitNeverRunsUnlocked(t *testing.T) {
	setGateTimeout(t, "1s")
	a := trainSetup(t)
	cmd, runs := countingGate(t)
	a.Cfg.Train.Lint = config.Lint{Cmd: cmd, Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "lint"})
	must(t, err)
	state := hookState(t, tk.Worktree)
	holdLock(t, state, "lock", "pre-commit hook in t12")
	holdLock(t, state, lintLockName, "")
	start := time.Now()
	must(t, a.lintDone(tk))
	if took := time.Since(start); took > 1800*time.Millisecond {
		t.Fatalf("lintDone waited %s: more than one GateTimeout (1s)", took)
	}
	if n := runs(); n != 0 {
		t.Fatalf("gate ran %d times without the lock", n)
	}
	evs, err := a.Store.Events(1000)
	must(t, err)
	var deferred string
	for _, e := range evs {
		if e.Task == tk.ID && e.Kind == EventLintDeferred {
			deferred = e.Data
		}
	}
	if !strings.Contains(deferred, "t12") {
		t.Fatalf("no %s event naming the holder: %q", EventLintDeferred, deferred)
	}
}

// #799: a free lock is taken at once, and the gate runs under it.
func TestLintDoneRunsWhenTheLockIsFree(t *testing.T) {
	setGateTimeout(t, "1s")
	a := trainSetup(t)
	cmd, runs := countingGate(t)
	a.Cfg.Train.Lint = config.Lint{Cmd: cmd, Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "lint"})
	must(t, err)
	must(t, a.lintDone(tk))
	if n := runs(); n != 1 {
		t.Fatalf("gate ran %d times", n)
	}
}

// #799: a waiter blocks in the kernel, so it gets the lock as soon as the
// holder lets go rather than on its next poll.
func TestLockNamedTakesTheLockWhenFreed(t *testing.T) {
	state := t.TempDir()
	f, err := os.OpenFile(filepath.Join(state, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	must(t, err)
	defer f.Close()
	must(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX))
	got := make(chan bool, 1)
	go func() {
		unlock, ok, _ := lockNamed(state, "lock", time.Now().Add(5*time.Second), "me", nil)
		defer unlock()
		got <- ok
	}()
	time.Sleep(100 * time.Millisecond)
	must(t, syscall.Flock(int(f.Fd()), syscall.LOCK_UN))
	select {
	case ok := <-got:
		if !ok {
			t.Fatal("lock not taken")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter never took the freed lock")
	}
	if h := lockHolder(state, "lock"); !strings.Contains(h, "me") {
		t.Fatalf("holder = %q", h)
	}
}
