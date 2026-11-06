package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
)

// quotaGate is a lint gate that fails on the environment until the file it
// returns exists, then passes.
func quotaGate(t *testing.T) (cmd, fixed string) {
	t.Helper()
	fixed = filepath.Join(t.TempDir(), "fixed")
	return "if [ -e " + shellQuote(fixed) + " ]; then exit 0; fi; echo 'write /x: Disk quota exceeded'; exit 1", fixed
}

// #785: a done gate that fails on the environment tells the orchestrator,
// holds the task as done-pending-env with a retry scheduled, and tells the
// worker to wait, not to fix the environment. Once the environment is
// back, the retry queues the branch with no done call from the worker.
func TestDoneGateEnvFailureTellsOrchestratorAndRetries(t *testing.T) {
	a := trainSetup(t)
	defer func(d time.Duration) { gateEnvPause = d }(gateEnvPause)
	gateEnvPause = time.Millisecond
	var swept []string
	defer func(f func(*App, string)) { envSweep = f }(envSweep)
	envSweep = func(_ *App, task string) { swept = append(swept, task) }

	cmd, fixed := quotaGate(t)
	a.Cfg.Train.Lint = config.Lint{Cmd: cmd, Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "quota"})
	must(t, err)
	write(t, tk.Worktree, "x.go", "x\n")
	commitAll(t, tk.Worktree, "x")
	_, _ = a.Store.TakeNotices(OrchestratorID, false)

	job, err := a.DoneAsync(tk.ID, "the work")
	must(t, err)
	if !job.Running {
		t.Fatalf("done ran inline: %+v", job)
	}
	a.WaitDoneJobs()
	if inTrain(t, a, tk.ID) {
		t.Fatal("queued on a red gate")
	}

	orch := noticeTexts(t, a, OrchestratorID)
	if !strings.Contains(orch, "action: ") || !strings.Contains(orch, tk.ID) || !strings.Contains(strings.ToLower(orch), "disk quota exceeded") {
		t.Fatalf("orchestrator not told of the environment failure: %q", orch)
	}
	worker := noticeTexts(t, a, tk.ID)
	if strings.Contains(worker, "Free ") || strings.Contains(worker, "call the saddle done tool again") {
		t.Fatalf("worker told to fix the environment or call done again: %q", worker)
	}
	if !strings.Contains(worker, "wait") {
		t.Fatalf("worker not told to wait: %q", worker)
	}
	if len(swept) == 0 {
		t.Fatal("saddle did not sweep scratch on the environment failure")
	}

	p, ok := a.pendingEnv()[tk.ID]
	if !ok {
		t.Fatal("no retry scheduled")
	}
	if !p.Next.After(time.Now()) || p.Summary != "the work" {
		t.Fatalf("pending entry %+v", p)
	}
	if !hasEvent(t, a, EventDonePendingEnv, "") {
		t.Fatal("no done_pending_env event")
	}

	// Not due yet: nothing runs.
	acted, err := a.DonePendingEnvTick(time.Now())
	must(t, err)
	if len(acted) != 0 {
		t.Fatalf("retried before its backoff: %v", acted)
	}

	// Still broken at the first retry: the backoff grows, the orchestrator
	// isn't told again.
	_, _ = a.Store.TakeNotices(OrchestratorID, false)
	acted, err = a.DonePendingEnvTick(p.Next.Add(time.Second))
	must(t, err)
	if len(acted) != 1 {
		t.Fatalf("due retry not run: %v", acted)
	}
	a.WaitDoneJobs()
	q := a.pendingEnv()[tk.ID]
	if q.Attempts != p.Attempts+1 || time.Until(q.Next) <= p.Next.Sub(p.Since) {
		t.Fatalf("backoff did not grow: %+v then %+v", p, q)
	}
	if n := noticeTexts(t, a, OrchestratorID); strings.Contains(n, "action: ") {
		t.Fatalf("orchestrator paged again on the same outage: %q", n)
	}

	// The environment is back: the next retry queues the branch.
	must(t, os.WriteFile(fixed, nil, 0o644))
	_, err = a.DonePendingEnvTick(q.Next.Add(time.Second))
	must(t, err)
	a.WaitDoneJobs()
	if !inTrain(t, a, tk.ID) {
		t.Fatal("the retry did not queue the branch")
	}
	if _, ok := a.pendingEnv()[tk.ID]; ok {
		t.Fatal("pending entry left after the branch queued")
	}
}

// #785: the orphan watch leaves a done-pending-env task to saddle's retry:
// nudging its worker to call done would undo "wait".
func TestOrphanWatchLeavesDonePendingEnvAlone(t *testing.T) {
	if cancelledDone[EventDonePendingEnv] || !doneOutcomes[EventDonePendingEnv] {
		t.Fatal("done_pending_env must end a done call without counting as cancelled")
	}
}

// #785: the retry gives up quietly on a task that finished meanwhile.
func TestDonePendingEnvDropsFinishedTasks(t *testing.T) {
	a := trainSetup(t)
	tk, err := a.Spawn(SpawnReq{Title: "gone"})
	must(t, err)
	must(t, a.setPendingEnv(tk.ID, func(p *PendingEnv) { p.Next = time.Now() }))
	must(t, a.Kill(tk.ID, false))
	acted, err := a.DonePendingEnvTick(time.Now().Add(time.Hour))
	must(t, err)
	if len(acted) != 0 {
		t.Fatalf("retried a killed task: %v", acted)
	}
	if _, ok := a.pendingEnv()[tk.ID]; ok {
		t.Fatal("killed task still pending")
	}
}

func TestEnvRetryBackoff(t *testing.T) {
	for n, want := range map[int]time.Duration{0: time.Minute, 1: 2 * time.Minute, 3: 8 * time.Minute, 10: envRetryMax} {
		if got := envRetryBackoff(n); got != want {
			t.Errorf("backoff(%d) = %s, want %s", n, got, want)
		}
	}
}
