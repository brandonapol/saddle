//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/mcpserver"
)

// TestJourneyGateHangTimesOut (#269): the test gate hangs on t1 (a sleep
// that never ends, run in the background so only its group can stop it).
// `saddle land` kills it with everything it started once it runs past
// [train] gate_timeout, returns t1 to its producer with "gate timed out
// after", lets go of train.lock, and lands t2 in the same run. Then a land
// interrupted with SIGTERM mid-gate takes its gate down too, and the next
// land goes through.
func TestJourneyGateHangTimesOut(t *testing.T) {
	scratch := t.TempDir()
	hang, child := filepath.Join(scratch, "hang"), filepath.Join(scratch, "child")
	gate := fmt.Sprintf("if [ -e %[1]s ]; then rm %[1]s; sleep 600 & echo $! > %[2]s; echo gate-hanging; wait; fi; true", shq(hang), shq(child))
	w := world(t, Options{TestCmd: gate, Tables: "[train]\nno_auto_rebase = true\ngate_timeout = \"2s\"\n"})
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, finished("alpha", "alpha\n")...)
	// The one-shot hang belongs to the first queued task. Wait for t1's
	// done call before starting t2, whose worker could otherwise finish first.
	w.WaitTask("t1", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	w.Spawn("t2", "Beta work", []string{"beta/**"}, finished("beta", "beta\n")...)
	w.WaitTask("t2", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	must(t, os.WriteFile(hang, nil, 0o644))

	start := time.Now()
	r := w.Saddle("land")
	if took := time.Since(start); took > Timeout() {
		t.Fatalf("land took %s on a 2s gate timeout\n%s", took, r)
	}
	if v := w.Task("t1"); v.Status == "landed" || !strings.HasPrefix(v.Train, "test_failed") {
		t.Fatalf("t1 after its gate hung: %+v\n%s", v, r)
	}
	told := w.AgentLog("t1")
	ns, err := w.App().Store.PeekNotices("t1", false)
	must(t, err)
	for _, n := range ns {
		told += n.Text
	}
	for _, want := range []string{"gate-hanging", "gate timed out after 2s"} {
		if !strings.Contains(told, want) {
			t.Fatalf("t1 wasn't told %q:\n%s", want, told)
		}
	}
	if v := w.Task("t2"); v.Status != "landed" {
		t.Fatalf("t2 didn't land after t1's gate timed out: %+v\n%s", v, r)
	}
	waitGone(t, child)

	// Interrupted mid-gate, land takes the gate's group down with it.
	w.Spawn("t3", "Gamma work", []string{"gamma/**"}, finished("gamma", "gamma\n")...)
	w.WaitTask("t3", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	must(t, os.Remove(child))
	must(t, os.WriteFile(hang, nil, 0o644))
	w.WriteConfig(Options{TestCmd: gate, Tables: "[train]\nno_auto_rebase = true\n"}) // the 20m default
	land := exec.Command(w.Bins.Saddle, "land")
	land.Dir, land.Env = w.Repo, w.Env()
	must(t, land.Start())
	Eventually(t, "the gate's child started", func() error {
		if b, _ := os.ReadFile(child); len(strings.TrimSpace(string(b))) == 0 {
			return fmt.Errorf("no pid in %s", child)
		}
		return nil
	})
	must(t, land.Process.Signal(syscall.SIGTERM))
	_ = land.Wait()
	waitGone(t, child)

	r = w.Saddle("land")
	if v := w.Task("t3"); v.Status != "landed" {
		t.Fatalf("t3 didn't land after the interrupted land: %+v\n%s", v, r)
	}
}

// waitGone waits until the process whose pid is in file has exited.
func waitGone(t *testing.T, file string) {
	t.Helper()
	b, err := os.ReadFile(file)
	must(t, err)
	var pid int
	if _, err := fmt.Sscan(string(b), &pid); err != nil {
		t.Fatalf("pid file %s: %q", file, b)
	}
	Eventually(t, fmt.Sprintf("gate child %d gone", pid), func() error {
		if err := syscall.Kill(pid, 0); err == nil || err == syscall.EPERM {
			return fmt.Errorf("pid %d still running", pid)
		}
		return nil
	})
}
