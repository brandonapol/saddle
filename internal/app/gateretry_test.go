package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
)

// hangOnce hangs the first time it runs (past the gate timeout) and passes
// after, counting runs in the returned file.
func hangOnce(t *testing.T) (cmd string, runs func() int) {
	t.Helper()
	dir := t.TempDir()
	log, once := filepath.Join(dir, "runs"), filepath.Join(dir, "once")
	cmd = "echo run >> " + shellQuote(log) + "; if [ ! -e " + shellQuote(once) + " ]; then touch " + shellQuote(once) + "; sleep 600; fi"
	return cmd, func() int { return countRuns(t, log) }
}

func countRuns(t *testing.T, log string) int {
	t.Helper()
	b, _ := os.ReadFile(log)
	return strings.Count(string(b), "run")
}

// #430: a done gate that only timed out is retried once; the second run
// passing lets done through.
func TestLintDoneRetriesATimeoutOnce(t *testing.T) {
	a := trainSetup(t)
	setGateTimeout(t, "1s")
	cmd, runs := hangOnce(t)
	a.Cfg.Train.Lint = config.Lint{Cmd: cmd, Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "lint"})
	must(t, err)
	must(t, a.lintDone(tk))
	if n := runs(); n != 2 {
		t.Fatalf("gate ran %d times, want 2", n)
	}
}

// #430: a gate that times out twice is still refused: one retry, not a loop.
func TestLintDoneRefusesASecondTimeout(t *testing.T) {
	a := trainSetup(t)
	setGateTimeout(t, "300ms")
	log := filepath.Join(t.TempDir(), "runs")
	a.Cfg.Train.Lint = config.Lint{Cmd: "echo run >> " + shellQuote(log) + "; sleep 600", Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "lint"})
	must(t, err)
	err = a.lintDone(tk)
	if err == nil || !strings.Contains(err.Error(), "gate timed out") {
		t.Fatalf("done on a gate that always hangs: %v", err)
	}
	if n := countRuns(t, log); n != 2 {
		t.Fatalf("gate ran %d times, want 2", n)
	}
}

// #430: a real failure is not retried.
func TestLintDoneDoesNotRetryARealFailure(t *testing.T) {
	a := trainSetup(t)
	setGateTimeout(t, "5s")
	log := filepath.Join(t.TempDir(), "runs")
	a.Cfg.Train.Lint = config.Lint{Cmd: "echo run >> " + shellQuote(log) + "; echo FAIL: assertion; exit 1", Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "lint"})
	must(t, err)
	if err := a.lintDone(tk); err == nil {
		t.Fatal("a red gate passed")
	}
	if n := countRuns(t, log); n != 1 {
		t.Fatalf("gate ran %d times, want 1", n)
	}
}

// #430: the train's gate runner retries a timeout once too, and not a
// real failure.
func TestRunGateEnvRetriesATimeoutOnce(t *testing.T) {
	a := trainSetup(t)
	setGateTimeout(t, "1s")
	cmd, runs := hangOnce(t)
	res := a.RunGateEnv(context.Background(), "t1", ShellGate(a.Root, cmd, a.GateTimeout()))
	if res.Err != nil || runs() != 2 {
		t.Fatalf("err = %v, runs = %d; want a pass on the second run", res.Err, runs())
	}
	log := filepath.Join(t.TempDir(), "runs")
	res = a.RunGateEnv(context.Background(), "t1", ShellGate(a.Root, "echo run >> "+shellQuote(log)+"; echo FAIL; exit 1", a.GateTimeout()))
	if res.Err == nil || countRuns(t, log) != 1 {
		t.Fatalf("real failure: err = %v, runs = %d; want one run", res.Err, countRuns(t, log))
	}
}
