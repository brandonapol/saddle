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
