//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/e2e/fakegh"
)

// startupBudget is how long saddle up may take from exec to its first frame
// in the journey below (#635). Measured at about 0.3s idle and under 1.5s
// at load 30 on 8 CPUs; the margin is for CI. E2E_STARTUP_BUDGET (a Go
// duration) overrides it.
func startupBudget() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("E2E_STARTUP_BUDGET")); err == nil && d > 0 {
		return d
	}
	return 8 * time.Second
}

// traceStage is when the named stage ended, in ms since process start, from
// a SADDLE_TRACE_STARTUP file, or false while it hasn't.
func traceStage(path, stage string) (int, bool) {
	b, _ := os.ReadFile(path)
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 || strings.Join(f[2:], " ") != stage {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(f[0], "ms"))
		return n, err == nil
	}
	return 0, false
}

// TestJourneyStartupFirstFrameNeverWaitsOnGitHub (#635): saddle up, doctor
// on, draws its first frame while GitHub answers nothing, within the
// startup budget. The doctor's GitHub checks still run, after the frame,
// and finish once GitHub answers. It times one stage from saddle's own
// trace and polls for every condition, so load slows it but can't fail it
// short of the budget.
func TestJourneyStartupFirstFrameNeverWaitsOnGitHub(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{})
	hold := func(prefixes []string) {
		must(t, w.GH.Update(func(s *fakegh.State) error { s.Hold = prefixes; return nil }))
	}
	hold([]string{""})
	t.Cleanup(func() { hold(nil) }) // before the world's cleanup, so no gh stays parked

	trace := filepath.Join(t.TempDir(), "startup-trace")
	cmd := "env SADDLE_TRACE_STARTUP=" + shq(trace) + " " + shq(w.Bins.Saddle) + ` up; echo "[saddle up exited $?]"; exec cat`
	must(t, w.Tmux.NewSession(tuiSession, 160, 45, w.Repo, cmd))
	u := &TUI{w: w, Target: tuiSession + ":0"}
	u.WaitScreen("orchestrator")

	var at int
	Eventually(t, "the trace to record the first frame", func() error {
		var ok bool
		if at, ok = traceStage(trace, "first frame"); !ok {
			b, _ := os.ReadFile(trace)
			return errorf("trace:\n%s", b)
		}
		return nil
	})
	if budget := startupBudget(); time.Duration(at)*time.Millisecond > budget {
		b, _ := os.ReadFile(trace)
		t.Fatalf("first frame at %dms, over the %s budget:\n%s", at, budget, b)
	}
	t.Logf("first frame at %dms", at)

	Eventually(t, "the doctor's GitHub checks to start after the first frame", func() error {
		if s := w.GHState(); len(s.Held) == 0 {
			return errorf("no gh call yet")
		}
		return nil
	})
	if _, ok := traceStage(trace, "doctor (full) [after first frame]"); ok {
		t.Fatal("the doctor finished while GitHub answered nothing")
	}
	hold(nil)
	Eventually(t, "the doctor to finish once GitHub answers", func() error {
		if _, ok := traceStage(trace, "doctor (full) [after first frame]"); !ok {
			b, _ := os.ReadFile(trace)
			return errorf("trace:\n%s", b)
		}
		return nil
	})
	u.Quit()
}
