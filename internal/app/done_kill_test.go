package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
)

// #784: a task killed while its background done gate runs stays killed
// when the gate passes. The gate must not mark it done or queue it.
func TestDoneAsyncKilledMidGateStaysKilled(t *testing.T) {
	for _, keep := range []bool{true, false} {
		name := "cleanup"
		if keep {
			name = "keep"
		}
		t.Run(name, func(t *testing.T) {
			a := trainSetup(t)
			cmd, runs, release := slowGate(t)
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
			if !waitUntil(func() bool { return runs() == 1 }) {
				t.Fatalf("gate never started (runs=%d)", runs())
			}
			must(t, a.Kill(tk.ID, keep))
			release()
			a.WaitDoneJobs()

			got, err := a.Store.Task(tk.ID)
			must(t, err)
			if got.Status != store.Killed {
				t.Fatalf("status after the gate passed: %s, want killed", got.Status)
			}
			if inTrain(t, a, tk.ID) {
				t.Fatal("the gate queued a killed task")
			}
			if n := noticeTexts(t, a, OrchestratorID); strings.Contains(n, "is done and queued") {
				t.Fatalf("orchestrator told a killed task is queued: %q", n)
			}
		})
	}
}
