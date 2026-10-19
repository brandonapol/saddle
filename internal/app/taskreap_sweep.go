package app

import (
	"fmt"
	"slices"

	"github.com/brandonapol/saddle/internal/procreap"
	"github.com/brandonapol/saddle/internal/store"
)

// reapTask kills every process still tagged with task id (#690): whatever
// an agent backgrounded out of its process group. It runs when a task ends
// (no_change, done, killed, landed) and never on a handoff or takeover,
// whose successor reuses the task. Callers' own ancestors are spared, so an
// agent calling done is not killed under itself.
func (a *App) reapTask(id string) {
	if n := procreap.ReapTask(a.Root, id, runKillGrace); n > 0 {
		a.Store.Event(id, "task_procs_reaped", fmt.Sprintf("%d process(es) tagged %s", n, id))
	}
}

// orphanStatus reports whether a task in this state can no longer own
// processes. Done is not one: a failed landing sends it back to its agent.
func orphanStatus(s string) bool {
	return s == store.Landed || s == store.Killed || s == store.NoChange
}

// OrphanTaskProcs maps each ended task to the pids still tagged with it.
// Processes tagged with a task this store doesn't know are left alone.
func (a *App) OrphanTaskProcs() map[string][]int {
	out := map[string][]int{}
	for id, pids := range procreap.Tagged(a.Root) {
		if t, err := a.Store.Task(id); err == nil && orphanStatus(t.Status) {
			out[id] = pids
		}
	}
	return out
}

// TaskSweep is what SweepOrphanTasks killed.
type TaskSweep struct {
	Tasks  []string // ended tasks that still had processes, sorted
	Killed int
}

// SweepOrphanTasks kills the processes of tasks that already ended, which
// catches leaks from before reaping on every end state existed (#690).
func (a *App) SweepOrphanTasks() TaskSweep {
	var res TaskSweep
	for id := range a.OrphanTaskProcs() {
		res.Tasks = append(res.Tasks, id)
	}
	slices.Sort(res.Tasks)
	for _, id := range res.Tasks {
		res.Killed += procreap.ReapTask(a.Root, id, runKillGrace)
	}
	if res.Killed > 0 {
		a.Store.Event("", "task_orphans_swept", fmt.Sprintf("%d process(es) of ended tasks %v", res.Killed, res.Tasks))
	}
	return res
}
