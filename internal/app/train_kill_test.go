package app

import (
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// #784: a task killed while the train tests it stays killed when its gate
// fails. The train must not mark it Conflict (a live status the resume
// watcher would try to bring back) or leave it in the train.
func TestLandKilledMidGateStaysKilled(t *testing.T) {
	a := trainSetup(t)
	gate, runs, release := slowGate(t)
	a.Cfg.Test.Cmd = gate + "; exit 1"
	tk := queueTask(t, a, "t1", "doomed", map[string]string{"one.txt": "one\n"})

	done := make(chan []LandResult, 1)
	go func() {
		rs, err := a.Land()
		if err != nil {
			t.Error(err)
		}
		done <- rs
	}()
	if !waitUntil(func() bool { return runs() == 1 }) {
		t.Fatalf("test gate never started (runs=%d)", runs())
	}
	must(t, a.Kill(tk.ID, true))
	release()
	<-done

	got, err := a.Store.Task(tk.ID)
	must(t, err)
	if got.Status != store.Killed {
		t.Fatalf("status after a failed land: %s, want killed", got.Status)
	}
	es, err := a.Store.Train()
	must(t, err)
	for _, e := range es {
		if e.Task == tk.ID && e.State != store.TrainOK {
			t.Fatalf("killed task still in the train: %+v", e)
		}
	}
	if ns, _ := a.Store.PeekNotices(tk.ID, false); len(ns) != 0 {
		t.Fatalf("killed task was told to fix its branch: %+v", ns)
	}
}

// #784: a killed task that is somehow still queued is dropped from the
// train, not landed or set to Conflict.
func TestLandSkipsAKilledTaskStillQueued(t *testing.T) {
	a := trainSetup(t)
	a.Cfg.Test.Cmd = "exit 1"
	tk := queueTask(t, a, "t1", "doomed", map[string]string{"one.txt": "one\n"})
	must(t, a.Store.SetStatus(tk.ID, store.Killed)) // killed, its dequeue lost
	_, err := a.Land()
	must(t, err)
	got, err := a.Store.Task(tk.ID)
	must(t, err)
	if got.Status != store.Killed {
		t.Fatalf("status: %s, want killed", got.Status)
	}
	if inTrain(t, a, tk.ID) {
		t.Fatal("killed task still in the train")
	}
}
