package app

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/runq"
)

// heavyTest points the queue at a private file and writes the repo's
// .saddle/runq.toml.
func heavyTest(t *testing.T, a *App, toml string) string {
	t.Helper()
	db := filepath.Join(t.TempDir(), "runq.db")
	t.Setenv(runq.EnvPath, db)
	t.Setenv(runq.EnvBypass, "")
	t.Setenv(runq.EnvLease, "")
	write(t, a.Root, ".saddle/runq.toml", toml)
	return db
}

func eventKinds(t *testing.T, a *App, prefix string) map[string]string {
	t.Helper()
	evs, err := a.Store.Events(100)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range evs {
		if strings.HasPrefix(e.Kind, prefix) {
			out[e.Kind] = e.Data
		}
	}
	return out
}

// TestRunHeavyLogsEvents: a run that queues logs run_queued, run_started
// with its wait and run_finished with its duration and wait.
func TestRunHeavyLogsEvents(t *testing.T) {
	a, _ := setup(t)
	db := heavyTest(t, a, "mode = \"enforce\"\n[classes.go-test]\nslots = 1\n")
	t.Setenv("SADDLE_TASK", "t7")
	q, err := runq.Open(runq.Options{Path: db, Mode: runq.ModeEnforce, Slots: map[string]int{}})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	h, err := q.Acquire(context.Background(), runq.Request{Class: "go-test", Label: "t83", Cmd: "make check"})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = h.Release()
	}()
	if err := a.RunHeavy(context.Background(), "go-test", runq.PrioGate, exec.Command("true")); err != nil {
		t.Fatal(err)
	}
	ev := eventKinds(t, a, "run_")
	if !strings.Contains(ev[EventRunQueued], "go-test") || !strings.Contains(ev[EventRunQueued], "t83") {
		t.Errorf("run_queued %q", ev[EventRunQueued])
	}
	if !strings.Contains(ev[EventRunStarted], "waited") {
		t.Errorf("run_started %q", ev[EventRunStarted])
	}
	if f := ev[EventRunFinished]; !strings.Contains(f, "go-test ok in") || !strings.Contains(f, "waited") || !strings.Contains(f, "enforce") {
		t.Errorf("run_finished %q", f)
	}
}

// TestRunHeavyObservesByDefault: with no runq.toml the run never waits and
// its history says observe.
func TestRunHeavyObservesByDefault(t *testing.T) {
	a, _ := setup(t)
	heavyTest(t, a, "")
	h := a.Heavy()
	q, _, err := h.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if q.Mode() != runq.ModeObserve {
		t.Fatalf("mode %q", q.Mode())
	}
	hold, err := q.Acquire(context.Background(), runq.Request{Class: "e2e"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Release() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := h.Run(ctx, HeavyOpts{Class: "e2e"}, exec.Command("true"))
	if err != nil || res.Waited > time.Second || res.Mode != runq.ModeObserve {
		t.Fatalf("result %+v err %v", res, err)
	}
	if ev := eventKinds(t, a, "run_"); !strings.Contains(ev[EventRunFinished], "observe") {
		t.Fatalf("events %v", ev)
	}
}

// TestHeavyBadConfigFailsOpen: a broken runq.toml warns and runs with the
// defaults instead of blocking every heavy run.
func TestHeavyBadConfigFailsOpen(t *testing.T) {
	a, _ := setup(t)
	heavyTest(t, a, "mode = [")
	var warn bytes.Buffer
	h := a.Heavy()
	h.Warn = &warn
	if _, err := h.Run(context.Background(), HeavyOpts{Class: "go-test"}, exec.Command("true")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), "runq.toml") {
		t.Fatalf("warning %q", warn.String())
	}
}

// TestHeavyUnusableQueueFailsOpen: if the queue can't be opened at all the
// command still runs, with a warning.
func TestHeavyUnusableQueueFailsOpen(t *testing.T) {
	a, _ := setup(t)
	heavyTest(t, a, "")
	blocker := filepath.Join(t.TempDir(), "file")
	write(t, filepath.Dir(blocker), "file", "x")
	t.Setenv(runq.EnvPath, filepath.Join(blocker, "sub", "runq.db"))
	marker := filepath.Join(t.TempDir(), "ran")
	var warn bytes.Buffer
	h := a.Heavy()
	h.Warn = &warn
	if _, err := h.Run(context.Background(), HeavyOpts{Class: "go-test"}, exec.Command("touch", marker)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("command did not run")
	}
	if !strings.Contains(warn.String(), "unqueued") {
		t.Fatalf("warning %q", warn.String())
	}
}

// TestCleanStaleLeases: saddle up's startup pass reaps leases whose holder
// is gone and logs it.
func TestCleanStaleLeases(t *testing.T) {
	a, _ := setup(t)
	db := heavyTest(t, a, "stale_after = \"1ms\"\n")
	q, err := runq.Open(runq.Options{Path: db, Mode: runq.ModeEnforce, Heartbeat: time.Hour, StaleAfter: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	l, err := q.Acquire(context.Background(), runq.Request{Class: "go-test", Label: "ghost"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()
	time.Sleep(10 * time.Millisecond) // its heartbeat is now older than stale_after
	var out bytes.Buffer
	a.CleanStaleLeases(&out)
	if !strings.Contains(out.String(), "1 stale") {
		t.Fatalf("output %q", out.String())
	}
	if ev := eventKinds(t, a, "run"); !strings.Contains(ev[EventRunqReaped], "1") {
		t.Fatalf("events %v", ev)
	}
	out.Reset()
	a.CleanStaleLeases(&out)
	if out.Len() != 0 {
		t.Fatalf("second pass said %q", out.String())
	}
}
