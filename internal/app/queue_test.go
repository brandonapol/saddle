package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// queued spawns n tasks, each committing its own file, and queues them.
func queued(t *testing.T, a *App, n int) []store.Task {
	t.Helper()
	var out []store.Task
	for i := range n {
		tk, err := a.Spawn(SpawnReq{Title: "task " + string(rune('a'+i))})
		must(t, err)
		write(t, tk.Worktree, tk.ID+".txt", tk.ID+"\n")
		commitAll(t, tk.Worktree, tk.ID)
		must(t, a.Done(tk.ID, tk.ID))
		out = append(out, tk)
	}
	return out
}

func queueIDs(t *testing.T, a *App) []string {
	t.Helper()
	q, err := a.Queue()
	must(t, err)
	var out []string
	for _, e := range q {
		out = append(out, e.Task)
	}
	return out
}

// #25: reorder moves a queued entry, and land follows the new order.
func TestMoveInQueueReordersLanding(t *testing.T) {
	a := trainSetup(t)
	ts := queued(t, a, 3)
	must(t, a.MoveInQueue(ts[2].ID, 1))
	if got := strings.Join(queueIDs(t, a), ","); got != ts[2].ID+","+ts[0].ID+","+ts[1].ID {
		t.Fatalf("queue = %s", got)
	}
	must(t, a.MoveInQueue(ts[2].ID, 99)) // past the end: to the back
	if got := strings.Join(queueIDs(t, a), ","); got != ts[0].ID+","+ts[1].ID+","+ts[2].ID {
		t.Fatalf("queue = %s", got)
	}
	must(t, a.MoveInQueue(ts[1].ID, 1))
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 3 || rs[0].Task != ts[1].ID || rs[1].Task != ts[0].ID {
		t.Fatalf("land order = %+v", rs)
	}
	if err := a.MoveInQueue(ts[0].ID, 1); err == nil {
		t.Fatal("moving a landed task: want error")
	}
}

// #25: a held entry is skipped by land, keeps its place, survives done, and
// lands once released.
func TestHoldSkipsLandUntilReleased(t *testing.T) {
	a := trainSetup(t)
	ts := queued(t, a, 2)
	must(t, a.Hold(ts[0].ID, "waiting on design review"))
	must(t, a.Done(ts[0].ID, "again")) // the agent calls done again: still held
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].Task != ts[1].ID || rs[0].State != store.TrainOK {
		t.Fatalf("land = %+v", rs)
	}
	q, _ := a.Queue()
	if len(q) != 1 || q[0].Task != ts[0].ID || q[0].State != store.OnHold || q[0].Note != "waiting on design review" {
		t.Fatalf("queue = %+v", q)
	}
	must(t, a.Unhold(ts[0].ID))
	rs, err = a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].Task != ts[0].ID || rs[0].State != store.TrainOK {
		t.Fatalf("land after release = %+v", rs)
	}
	if err := a.Hold(ts[0].ID, ""); err == nil {
		t.Fatal("holding a landed task: want error")
	}
	if err := a.Unhold(ts[1].ID); err == nil {
		t.Fatal("releasing a task that isn't held: want error")
	}
}
