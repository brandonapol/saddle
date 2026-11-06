package app

import (
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/brandonapol/saddle/internal/store"
)

// #797: a spawn that failed (here, git worktree add losing index.lock to the
// checkpoint watcher) leaves a failed row on its issue. That row is not live
// work: autopilot still picks the issue and the snapshot doesn't list it.
func TestFailedSpawnDoesNotBlockItsIssue(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	must(t, a.Store.CreateTask(store.Task{
		ID: "t90", Issue: 1, Role: store.RoleWorker, Status: StatusFailed,
		Summary: "worktree add: index.lock: File exists",
	}))
	now := time.Now().Add(time.Hour)
	d := a.NewAutopilot(readyQueue(apIssue(1, "claims: alpha/**")))
	d.Now = func() time.Time { return now }
	_, err := d.Enable(autopilot.Options{})
	must(t, err)
	rep, err := d.Tick()
	must(t, err)
	if len(rep.Spawned) != 1 || rep.Spawned[0].Issue != 1 {
		t.Fatalf("spawned %+v, skipped %v: the failed t90 still holds #1", rep.Spawned, rep.Skipped)
	}
	if why := rep.Skipped[1]; strings.Contains(why, "already works on it") {
		t.Fatalf("#1 skipped: %s", why)
	}
	snap, err := (&autopilotEnv{a: a}).Snapshot(now)
	must(t, err)
	for _, s := range snap.Tasks {
		if strings.HasPrefix(s, "t90 ") {
			t.Fatalf("snapshot lists the failed t90 as live work: %v", snap.Tasks)
		}
	}
}
