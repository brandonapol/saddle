package store

import (
	"testing"
)

func trainOrder(t *testing.T, s *Store) []string {
	t.Helper()
	es, err := s.Train()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Task+":"+e.State)
	}
	return out
}

// A held entry stays held, in its place, when its agent calls done again;
// only SetTrain (unhold) puts it back in line.
func TestEnqueueKeepsHold(t *testing.T) {
	s := openTest(t)
	for _, id := range []string{"t1", "t2"} {
		if err := s.Enqueue(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetTrain("t1", OnHold, "waiting on review", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Enqueue("t1"); err != nil {
		t.Fatal(err)
	}
	got := trainOrder(t, s)
	if len(got) != 2 || got[0] != "t1:on_hold" || got[1] != "t2:queued" {
		t.Fatalf("order = %v", got)
	}
}

// SetTrainOrder reorders the named entries over the seqs they already hold,
// leaving the others where they are.
func TestSetTrainOrder(t *testing.T) {
	s := openTest(t)
	for _, id := range []string{"t1", "t2", "t3", "t4"} {
		if err := s.Enqueue(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetTrain("t1", "landed", "a..b", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTrainOrder([]string{"t4", "t2", "t3"}); err != nil {
		t.Fatal(err)
	}
	got := trainOrder(t, s)
	// t1 moved to the back when it landed (#413) and stays there.
	want := []string{"t4:queued", "t2:queued", "t3:queued", "t1:landed"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
	if err := s.SetTrainOrder([]string{"t2", "nope"}); err == nil {
		t.Fatal("ordering an unknown task: want error")
	}
}

// #413: an entry that lands goes after everything already in the train, so
// train order is landing order even when a held entry lands after a later one.
func TestLandingMovesEntryToBack(t *testing.T) {
	s := openTest(t)
	for _, id := range []string{"t1", "t2", "t3"} {
		if err := s.Enqueue(id); err != nil {
			t.Fatal(err)
		}
	}
	steps := []struct{ task, state string }{
		{"t1", OnHold}, {"t2", TrainOK}, {"t1", Queued}, {"t1", TrainOK},
		{"t2", TrainOK}, // a landed entry rewritten (e.g. by restack) keeps its place
	}
	for _, st := range steps {
		if err := s.SetTrain(st.task, st.state, "", false); err != nil {
			t.Fatal(err)
		}
	}
	got := trainOrder(t, s)
	if len(got) != 3 || got[0] != "t3:queued" || got[1] != "t2:landed" || got[2] != "t1:landed" {
		t.Fatalf("order = %v", got)
	}
}
