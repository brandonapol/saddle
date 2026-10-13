package app

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// #432: a task's make check had a go test wedged for ~4h50m, far past its
// own -timeout 15m, and nothing landed. The machine was loaded too, but the
// watchdog must name the hung test, its pid and its task, not "overload".
func TestWatchdogNamesHungGateOverOverload(t *testing.T) {
	t.Parallel()
	wt := "/repo/.saddle/worktrees/t205-thing"
	procs := func() ([]Proc, error) {
		return []Proc{
			{PID: 1059100, Elapsed: 4*time.Hour + 51*time.Minute, Args: "make check", Cwd: wt},
			{PID: 1059178, Elapsed: 4*time.Hour + 50*time.Minute, Args: "go test ./internal/e2e/... -count=1 -timeout 15m", Cwd: wt},
			{PID: 1059300, Elapsed: 5 * time.Minute, Args: "go test ./... -timeout 15m", Cwd: "/elsewhere"}, // inside its timeout
			{PID: 42, Elapsed: 30 * time.Hour, Args: "/usr/bin/sleep infinity", Cwd: "/"},                   // not a gate
		}, nil
	}
	tasks := []store.Task{{ID: "t205", Role: store.RoleWorker, Worktree: wt}}
	hung := HungGates(procs, tasks)
	if len(hung) == 0 || hung[0].PID != 1059178 || hung[0].Task != "t205" {
		t.Fatalf("hung = %+v, want the go test (pid 1059178, task t205) first", hung)
	}
	for _, h := range hung {
		if h.PID == 1059300 || h.PID == 42 {
			t.Fatalf("hung lists %+v", h)
		}
	}
	rc := Diagnose(WatchStatus{Live: 2, PromptBlocked: 1, Load1: 99, CPUs: 4, Hung: hung})
	if rc.Key != CauseHungGate {
		t.Fatalf("cause = %q, want %q", rc.Key, CauseHungGate)
	}
	for _, want := range []string{"1059178", "t205", "15m"} {
		if !strings.Contains(rc.What, want) && !strings.Contains(rc.Fix, want) {
			t.Errorf("diagnosis %+v does not name %q", rc, want)
		}
	}
	if strings.Contains(rc.What, "overload") {
		t.Errorf("diagnosis blames overload: %q", rc.What)
	}
}

func TestGateTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args string
		want time.Duration
		ok   bool
	}{
		{"go test ./... -timeout 15m", 15 * time.Minute, true},
		{"go test -timeout=20m ./...", 20 * time.Minute, true},
		{"/tmp/go-build1/b001/e2e.test -test.paniconexit0 -test.timeout=15m0s -test.count=1", 15 * time.Minute, true},
		{"go test ./...", 10 * time.Minute, true}, // go's default
		{"go test -timeout 0 ./...", 0, false},    // no timeout: can't be past it
		{"make check", hungMakeCheck, true},
		{"/usr/bin/make -j4 check", hungMakeCheck, true},
		{"go build ./...", 0, false},
		{"vim go test", 0, false},
	} {
		if got, ok := gateTimeout(tc.args); ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("gateTimeout(%q) = %s, %v; want %s, %v", tc.args, got, ok, tc.want, tc.ok)
		}
	}
}

// psProcs, the real lister, sees this test process.
func TestPSProcsSeesSelf(t *testing.T) {
	t.Parallel()
	ps, err := psProcs()
	if err != nil {
		t.Skipf("ps: %v", err)
	}
	for _, p := range ps {
		if p.PID == os.Getpid() {
			return
		}
	}
	t.Fatalf("pid %d not in %d procs", os.Getpid(), len(ps))
}
