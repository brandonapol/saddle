package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/runq"
	"github.com/brandonapol/saddle/internal/store"
)

// #849: Kill cancels a done gate that is running, not just discards its
// result. The gate's process group exits, the heavy-run lease held on the
// task's behalf is freed, and the task stays killed.
func TestKillCancelsRunningDoneGate(t *testing.T) {
	a := trainSetup(t)
	db := heavyTest(t, a, gateRunqToml)
	pidFile := filepath.Join(t.TempDir(), "pid")
	// A gate that never finishes on its own.
	cmd := "echo $$ > " + shellQuote(pidFile) + "; while :; do sleep 0.05; done"
	a.Cfg.Train.Lint = config.Lint{Cmd: cmd, Set: true}
	tk, err := a.Spawn(SpawnReq{Title: "doomed"})
	must(t, err)
	write(t, tk.Worktree, "x.go", "ok\n")
	commitAll(t, tk.Worktree, "work")

	job, err := a.DoneAsync(tk.ID, "doomed")
	must(t, err)
	if !job.Running {
		t.Fatalf("done: %+v, want a background gate", job)
	}
	var pid int
	if !waitUntil(func() bool {
		b, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		return pid > 0
	}) {
		t.Fatal("gate never started")
	}
	t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
	// The heavy-run lease the gate's make check holds for the task (a shim
	// inside the gate takes it labeled with SADDLE_TASK).
	q := openQueue(t, db)
	lease, err := q.Acquire(context.Background(), runq.Request{Class: "go-test", Prio: runq.PrioWorker, Label: tk.ID, Repo: repoName(a.Root), Cmd: "go test ./..."})
	must(t, err)

	must(t, a.Kill(tk.ID, true))

	finished := make(chan struct{})
	go func() { a.WaitDoneJobs(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(15 * time.Second):
		t.Fatal("the done gate kept running after Kill")
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("gate process %d still alive after Kill: %v", pid, err)
	}
	select {
	case <-lease.Lost():
	case <-time.After(5 * time.Second):
		t.Fatal("the task's heavy-run lease was not freed by Kill")
	}
	st, err := q.Status()
	must(t, err)
	for _, c := range st {
		for _, e := range append(c.Holders, c.Waiters...) {
			if e.Label == tk.ID {
				t.Fatalf("lease for %s still in the queue: %+v", tk.ID, e)
			}
		}
	}
	got, err := a.Store.Task(tk.ID)
	must(t, err)
	if got.Status != store.Killed {
		t.Fatalf("status %s, want killed", got.Status)
	}
	if inTrain(t, a, tk.ID) {
		t.Fatal("a killed task was queued")
	}
	if n := noticeTexts(t, a, tk.ID); strings.Contains(n, "done gate") {
		t.Fatalf("killed task told about its gate: %q", n)
	}
}

// #849: the train's test gate for a task being killed loses its lease and
// stops, and the orchestrator isn't told the dead task "stays queued".
func TestKillTaskLeasesStopsTrainGateQuietly(t *testing.T) {
	a, _ := setup(t)
	heavyTest(t, a, gateRunqToml)
	must(t, a.Store.CreateTask(store.Task{ID: "t1", Title: "doomed", Role: store.RoleWorker, Status: store.Running}))
	must(t, a.Store.Enqueue("t1"))
	started := make(chan struct{})
	run := func(ctx context.Context, _ []string) (string, error) {
		close(started)
		<-ctx.Done()
		return "stopped", ctx.Err()
	}
	done := make(chan GateEnvResult, 1)
	go func() { done <- a.RunGateEnv(context.Background(), "t1", run) }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("gate never started")
	}
	must(t, a.Store.SetStatus("t1", store.Killed))
	a.killTaskLeases("t1")
	select {
	case res := <-done:
		if res.Err == nil {
			t.Fatalf("gate passed after its lease was killed: %+v", res)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the train gate kept running after its task's lease was killed")
	}
	if n := noticeTexts(t, a, OrchestratorID); strings.Contains(n, "stays queued") {
		t.Fatalf("orchestrator told a killed task stays queued: %q", n)
	}
}
