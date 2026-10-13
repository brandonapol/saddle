package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// #392: land never answers null, even with nothing to land.
func TestLandRecordedNeverNil(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	rs, err := a.LandRecorded()
	must(t, err)
	if rs == nil {
		t.Fatal("LandRecorded returned nil results")
	}
}

// #392: a task queued when land was called but landed by another train run
// while this one waited for the lock is still reported as landed.
func TestLandRecordedReportsConcurrentLanding(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	queueTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	queued := a.queuedTasks()
	if _, err := a.Land(); err != nil { // the other run
		t.Fatal(err)
	}
	rs := a.accountLandings(queued, nil)
	if len(rs) != 1 || rs[0].Task != "t1" || rs[0].State != store.TrainOK {
		t.Fatalf("results = %+v, want t1 landed", rs)
	}
}

// #392: every task land landed keeps a from..to train note; one left without
// a range is rebuilt from its landing.
func TestLandRecordedRestoresLandedRange(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	queueTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	queueTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"}, map[string]string{"two.txt": "two\nmore\n"})
	rs, err := a.Land()
	must(t, err)
	want := trainNote(t, a, "t2")
	_, to, _ := strings.Cut(want, "..")
	must(t, a.Store.SetTrain("t2", store.TrainOK, to, false))

	a.accountLandings(nil, rs)
	if got := trainNote(t, a, "t2"); got != want {
		t.Fatalf("t2 note = %q, want %q", got, want)
	}
}
