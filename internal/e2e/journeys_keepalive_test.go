//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
)

// enginePID is the pid holding the engine lock, 0 when nobody does.
func enginePID(w *World, a *app.App) int {
	if a.LockOwner() != app.LockEngine {
		return 0
	}
	b, _ := os.ReadFile(filepath.Join(w.Repo, ".saddle", "tui.lock.pid"))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// TestJourneyKilledEngineRevivedByNextMCPCall (#789): the plugin engine is
// killed outright (OOM, a stray kill -9). The next saddle tool call any
// agent makes starts it again, detached, and saddle plugin engine --stop
// stops it for good.
func TestJourneyKilledEngineRevivedByNextMCPCall(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{})
	a := w.App()
	startEngine(w)
	var first int
	Eventually(t, "the engine to hold its lock", func() error {
		if first = enginePID(w, a); first == 0 {
			return errorf("no engine lock yet")
		}
		return nil
	})
	if _, ok := a.EngineWanted(); !ok {
		t.Fatal("the engine left no keep-alive marker")
	}
	must(t, syscall.Kill(first, syscall.SIGKILL))
	Eventually(t, "the engine lock to free", func() error {
		if a.LockOwner() != "" {
			return errorf("lock still held by %s", a.LockOwner())
		}
		return nil
	})
	t.Cleanup(func() {
		a.StopEngine()
		_ = Poll(10*time.Second, func() error {
			if a.LockOwner() != "" {
				return errorf("engine still running")
			}
			return nil
		})
	})

	w.MustMCP("t0", "status", nil) // one call

	var revived int
	Eventually(t, "the engine to come back", func() error {
		if revived = enginePID(w, a); revived == 0 || revived == first {
			return errorf("engine pid %d", revived)
		}
		return nil
	})
	if pid, ok := a.EngineWanted(); !ok || pid == 0 || pid == revived {
		t.Fatalf("the revived engine should run under a supervisor that keeps the marker: marker pid %d (%v), engine %d", pid, ok, revived)
	}

	// Stopped on purpose, it stays down: another call starts nothing.
	w.MustSaddle("plugin", "engine", "--stop")
	Eventually(t, "the engine to stop", func() error {
		if a.LockOwner() != "" {
			return errorf("engine still running")
		}
		return nil
	})
	w.MustMCP("t0", "status", nil)
	// Nothing should happen, so there is no condition to wait on: give a
	// wrongly revived engine a second to take the lock before checking.
	time.Sleep(time.Second)
	if a.LockOwner() != "" {
		t.Fatal("an engine stopped on purpose was revived")
	}
}
