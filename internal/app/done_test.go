package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
)

// slowGate is a lint gate that logs each run, then blocks until release
// exists: a make check on a busy machine.
func slowGate(t *testing.T) (cmd string, runs func() int, release func()) {
	t.Helper()
	dir := t.TempDir()
	log, rel := filepath.Join(dir, "runs"), filepath.Join(dir, "release")
	cmd = "echo run >> " + shellQuote(log) + "; while [ ! -e " + shellQuote(rel) + " ]; do sleep 0.05; done"
	runs = func() int {
		b, _ := os.ReadFile(log)
		return strings.Count(string(b), "run")
	}
	release = func() { must(t, os.WriteFile(rel, nil, 0o644)) }
	t.Cleanup(release) // never leave a gate spinning
	return cmd, runs, release
}

func inTrain(t *testing.T, a *App, task string) bool {
	t.Helper()
	es, err := a.Store.Train()
	must(t, err)
	for _, e := range es {
		if e.Task == task {
			return true
		}
	}
	return false
}

func noticeTexts(t *testing.T, a *App, task string) string {
	t.Helper()
	ns, err := a.Store.PeekNotices(task, false)
	must(t, err)
	var b strings.Builder
	for _, n := range ns {
		b.WriteString(n.Kind + ": " + n.Text + "\n")
	}
	return b.String()
}

// #368: done returns at once with a job id while a slow gate runs in the
// background; a retry joins that job instead of starting a second check;
// the task is queued when the gate passes and the agent hears about it.
func TestDoneAsyncSlowGateQueuesAndRetryJoins(t *testing.T) {
	a := trainSetup(t)
	cmd, runs, release := slowGate(t)
	a.Cfg.Train.Lint = config.Lint{Cmd: cmd, Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "slow"})
	must(t, err)
	write(t, tk.Worktree, "x.go", "ok\n")
	commitAll(t, tk.Worktree, "work")

	start := time.Now()
	job, err := a.DoneAsync(tk.ID, "slow gate")
	must(t, err)
	if time.Since(start) > 5*time.Second {
		t.Fatalf("done blocked on the gate for %s", time.Since(start))
	}
	if !job.Running || job.ID == "" || job.Joined {
		t.Fatalf("first done: %+v", job)
	}
	if !waitUntil(func() bool { return runs() == 1 }) {
		t.Fatalf("gate never started (runs=%d)", runs())
	}
	if inTrain(t, a, tk.ID) {
		t.Fatal("queued before the gate passed")
	}

	again, err := a.DoneAsync(tk.ID, "retry")
	must(t, err)
	if !again.Running || !again.Joined || again.ID != job.ID {
		t.Fatalf("retry while the gate runs: %+v, want to join %s", again, job.ID)
	}

	release()
	a.WaitDoneJobs()
	if n := runs(); n != 1 {
		t.Fatalf("gate ran %d times; a retry must not start another", n)
	}
	if !inTrain(t, a, tk.ID) {
		t.Fatal("a passing slow gate did not queue the task")
	}
	got, err := a.Store.Task(tk.ID)
	must(t, err)
	if got.Status != store.Done || got.Summary != "slow gate" {
		t.Fatalf("task after the gate: status %s summary %q", got.Status, got.Summary)
	}
	if n := noticeTexts(t, a, tk.ID); !strings.Contains(n, "queued") || !strings.Contains(n, job.ID) {
		t.Fatalf("agent was not told the gate passed: %q", n)
	}

	// Once queued, done again needs no gate: the tree already passed.
	after, err := a.DoneAsync(tk.ID, "")
	must(t, err)
	if after.Running {
		t.Fatalf("done on a passed tree started a gate: %+v", after)
	}
	if n := runs(); n != 1 {
		t.Fatalf("gate ran %d times", n)
	}
}

// #368: a red background gate leaves the task unqueued and wakes the agent
// with the failure.
func TestDoneAsyncRedGateNotifiesAgent(t *testing.T) {
	a := trainSetup(t)
	a.Cfg.Train.Lint = config.Lint{Cmd: badLint, Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "red"})
	must(t, err)
	write(t, tk.Worktree, "x.go", "LINTBAD\n")
	commitAll(t, tk.Worktree, "bad")

	job, err := a.DoneAsync(tk.ID, "red")
	must(t, err)
	if !job.Running {
		t.Fatalf("done ran the gate inline: %+v", job)
	}
	a.WaitDoneJobs()
	if inTrain(t, a, tk.ID) {
		t.Fatal("a red gate queued the task")
	}
	n := noticeTexts(t, a, tk.ID)
	for _, want := range []string{"action: ", job.ID, "lint: needs fixing: ./x.go", "call the saddle done tool again"} {
		if !strings.Contains(n, want) {
			t.Errorf("notice %q lacks %q", n, want)
		}
	}

	// The job is over: the next done starts a fresh gate.
	write(t, tk.Worktree, "x.go", "ok\n")
	commitAll(t, tk.Worktree, "fix")
	next, err := a.DoneAsync(tk.ID, "fixed")
	must(t, err)
	if !next.Running || next.Joined || next.ID == job.ID {
		t.Fatalf("done after a failed job: %+v", next)
	}
	a.WaitDoneJobs()
	if !inTrain(t, a, tk.ID) {
		t.Fatal("fixed branch not queued")
	}
}

// #368: a commit made while the gate ran is not what the gate vouched for,
// so the job doesn't queue it.
func TestDoneAsyncRefusesATreeThatMovedDuringTheGate(t *testing.T) {
	a := trainSetup(t)
	cmd, runs, release := slowGate(t)
	a.Cfg.Train.Lint = config.Lint{Cmd: cmd, Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "moved"})
	must(t, err)
	write(t, tk.Worktree, "x.go", "ok\n")
	commitAll(t, tk.Worktree, "work")
	job, err := a.DoneAsync(tk.ID, "moved")
	must(t, err)
	if !waitUntil(func() bool { return runs() == 1 }) {
		t.Fatal("gate never started")
	}
	write(t, tk.Worktree, "y.go", "more\n")
	commitAll(t, tk.Worktree, "more work")
	release()
	a.WaitDoneJobs()
	if inTrain(t, a, tk.ID) {
		t.Fatal("queued a tree the gate never checked")
	}
	if n := noticeTexts(t, a, tk.ID); !strings.Contains(n, job.ID) || !strings.Contains(n, "call the saddle done tool again") {
		t.Fatalf("agent not told to call done again: %q", n)
	}
}
