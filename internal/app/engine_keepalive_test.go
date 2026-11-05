package app

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// fakeStart replaces StartEngine for the test and counts its calls.
func fakeStart(t *testing.T) *int {
	t.Helper()
	n := 0
	old := StartEngine
	StartEngine = func(string) error { n++; return nil }
	t.Cleanup(func() { StartEngine = old })
	return &n
}

// #789: an engine that died while meant to run is started again by the next
// call that checks, once, and never one stopped on purpose.
func TestReviveEngine(t *testing.T) {
	a, _ := setup(t)
	started := fakeStart(t)
	if a.ReviveEngine() || *started != 0 {
		t.Fatal("revived an engine nobody wanted")
	}
	dead := exec.Command("true")
	must(t, dead.Run())
	must(t, a.WantEngine(dead.Process.Pid)) // the engine, now gone
	if !a.ReviveEngine() || *started != 1 {
		t.Fatalf("dead engine not revived (started %d)", *started)
	}
	if a.ReviveEngine() || *started != 1 {
		t.Fatal("revived twice within the cooldown")
	}
	if !hasKeepaliveEvent(t, a, EventEngineRevived) {
		t.Fatal("no engine_revived event")
	}

	// Past the cooldown, but the lock is held: the engine runs.
	must(t, os.Truncate(a.engineRevivePath(), 0))
	rel, err := a.AcquireLock(LockEngine)
	must(t, err)
	if a.ReviveEngine() {
		t.Fatal("revived a running engine")
	}
	rel()

	// A live supervisor between restarts brings its own child back.
	sup := exec.Command("sleep", "60")
	must(t, sup.Start())
	t.Cleanup(func() { _ = sup.Process.Kill(); _ = sup.Wait() })
	time.Sleep(20 * time.Millisecond)
	must(t, a.WantEngine(sup.Process.Pid))
	if a.ReviveEngine() {
		t.Fatal("revived over a live supervisor")
	}

	// Stopped on purpose: the marker is gone and the keeper got SIGINT.
	if !a.StopEngine() {
		t.Fatal("StopEngine signalled nothing")
	}
	var st syscall.WaitStatus
	if _, err := syscall.Wait4(sup.Process.Pid, &st, 0, nil); err != nil || st.Signal() != syscall.SIGINT {
		t.Fatalf("supervisor not interrupted: %v %v", st, err)
	}
	if _, ok := a.EngineWanted(); ok {
		t.Fatal("marker survived StopEngine")
	}
	if a.ReviveEngine() || *started != 1 {
		t.Fatal("revived an engine stopped on purpose")
	}
}

func hasKeepaliveEvent(t *testing.T, a *App, kind string) bool {
	t.Helper()
	evs, err := a.Store.Events(200)
	must(t, err)
	for _, e := range evs {
		if e.Kind == kind {
			return true
		}
	}
	return false
}
