package store

import (
	"errors"
	"testing"
)

// #784: a finished task (killed, landed, no change) can't be moved back to a
// live status by an ordinary status write: only Revive brings it back.
func TestSetStatusRefusesToReviveAFinishedTask(t *testing.T) {
	s := openTest(t)
	for _, end := range []string{Killed, Landed, NoChange} {
		id := "t-" + end
		if err := s.CreateTask(Task{ID: id, Role: RoleWorker, Status: Running}); err != nil {
			t.Fatal(err)
		}
		if err := s.SetStatus(id, end); err != nil {
			t.Fatal(err)
		}
		for _, live := range []string{Conflict, Running, Idle, NeedsYou, Done} {
			if err := s.SetStatus(id, live); !errors.Is(err, ErrFinished) {
				t.Errorf("SetStatus(%s → %s) = %v, want ErrFinished", end, live, err)
			}
		}
		got, err := s.Task(id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != end {
			t.Fatalf("%s became %s", end, got.Status)
		}
		// Finished to finished is fine: a repair kills a landed original.
		if err := s.SetStatus(id, Killed); err != nil {
			t.Errorf("SetStatus(%s → killed) = %v", end, err)
		}
		if err := s.Revive(id, Done); err != nil {
			t.Fatalf("Revive(%s → done) = %v", end, err)
		}
		if got, _ := s.Task(id); got.Status != Done {
			t.Fatalf("after Revive: %s", got.Status)
		}
	}
	if err := s.SetStatus("nope", Running); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetStatus on a missing task = %v, want ErrNotFound", err)
	}
}

// #784: FinishDone marks a live task done and queues it, but leaves a
// killed one alone, so a gate racing a kill can't put it back in the train.
func TestFinishDoneRefusesAFinishedTask(t *testing.T) {
	s := openTest(t)
	for id, st := range map[string]string{"t1": Killed, "t2": Running} {
		if err := s.CreateTask(Task{ID: id, Role: RoleWorker, Status: st}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FinishDone("t1"); !errors.Is(err, ErrFinished) {
		t.Fatalf("FinishDone(killed) = %v, want ErrFinished", err)
	}
	if err := s.FinishDone("t2"); err != nil {
		t.Fatal(err)
	}
	es, err := s.Train()
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || es[0].Task != "t2" || es[0].State != Queued {
		t.Fatalf("train: %+v", es)
	}
	if got, _ := s.Task("t2"); got.Status != Done {
		t.Fatalf("t2 status %s, want done", got.Status)
	}
	if got, _ := s.Task("t1"); got.Status != Killed {
		t.Fatalf("t1 status %s, want killed", got.Status)
	}
}

// #797: a task whose spawn failed holds no claims and has no agent, so it is
// not active: it must not count as live work on its issue.
func TestFailedTaskIsNotActive(t *testing.T) {
	if (Task{Status: Failed}).Active() {
		t.Fatal("a failed task counts as active")
	}
	for _, live := range []string{Running, Idle, NeedsYou, Done, Conflict} {
		if !(Task{Status: live}).Active() {
			t.Errorf("%s is not active", live)
		}
	}
}

// #936: Reopen sets fixing, its reason and its fix count in one write and
// refuses a finished task; KillBy records what killed a task.
func TestReopenAndKillBy(t *testing.T) {
	s := openTest(t)
	for id, st := range map[string]string{"t1": PROpen, "t2": Merged} {
		if err := s.CreateTask(Task{ID: id, Role: RoleWorker, Status: st}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Reopen("t1", "ci: test failed", 1); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Task("t1"); got.Status != Fixing || got.FixReason != "ci: test failed" || got.FixAttempts != 1 {
		t.Fatalf("t1 = %s %q %d, want fixing with the reason and 1", got.Status, got.FixReason, got.FixAttempts)
	}
	if err := s.Reopen("t2", "ci: x", 1); !errors.Is(err, ErrFinished) {
		t.Fatalf("Reopen(merged) = %v, want ErrFinished", err)
	}
	if err := s.KillBy("t1", KilledByPRClosed); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Task("t1"); got.Status != Killed || got.KilledBy != KilledByPRClosed {
		t.Fatalf("t1 = %s killed_by %q", got.Status, got.KilledBy)
	}
	if !(Task{Status: Fixing}).Active() || !(Task{Status: GaveUp}).Active() {
		t.Fatal("fixing and gave_up tasks keep their worktrees: they must stay active")
	}
}
