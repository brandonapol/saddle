package app

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// floodEvents logs n "tool" events, the bulk of a long-lived state.db,
// and returns how many event rows the store had read before.
func floodEvents(a *App, n int) int64 {
	for i := range n {
		a.Store.Event(fmt.Sprintf("n%d", i%50), "tool", "Edit")
	}
	return a.Store.EventRowsRead()
}

const floodSize = 1500

// readFew fails t when the store read more than a handful of event rows
// since before: a whole-table load of the flood reads them all (#778).
func readFew(t *testing.T, a *App, before int64, what string) {
	t.Helper()
	if read := a.Store.EventRowsRead() - before; read >= floodSize/2 {
		t.Fatalf("%s read %d event rows of a %d-row flood; want only the kinds it needs", what, read, floodSize)
	}
}

// #778 follow-up: the hung watch reads only its keep and takeover events,
// and still honours a keep.
func TestHungTickReadsOnlyItsEvents(t *testing.T) {
	a, _, tk := hungWorker(t)
	must(t, a.KeepWorker(tk.ID, "leave it"))
	before := floodEvents(a, floodSize)
	now, window := time.Now().UTC(), a.StallConfig().HungAfter
	for i := range 5 {
		_, err := a.HungTick(now.Add(time.Duration(i) * window))
		must(t, err)
	}
	readFew(t, a, before, "HungTick")
	if es, err := a.Store.TaskEvents(tk.ID, EventHungTakeover); err != nil || len(es) != 0 {
		t.Fatalf("a kept worker was taken over: %+v, %v", es, err)
	}
	if got, err := a.Store.Task(tk.ID); err != nil || got.Window != tk.Window {
		t.Fatalf("a kept worker was relaunched: %+v, %v", got, err)
	}
}

// #778 follow-up: the done-orphan watch reads only done-outcome events, and
// still flags a cancelled done.
func TestOrphanTickReadsOnlyItsEvents(t *testing.T) {
	a, _, tk := orphanedWorker(t)
	before := floodEvents(a, floodSize)
	now := time.Now().UTC()
	_, err := a.OrphanTick(now)
	must(t, err)
	_, err = a.OrphanTick(now.Add(a.StallConfig().DoneOrphan + time.Second))
	must(t, err)
	readFew(t, a, before, "OrphanTick")
	if es, err := a.Store.TaskEvents(tk.ID, EventDoneOrphaned); err != nil || len(es) == 0 {
		t.Fatalf("the cancelled done was not flagged: %+v, %v", es, err)
	}
}

// #778 follow-up: the landed-with-no-PR alarm reads only alarm and landing
// events, and still raises once per landing.
func TestAlarmUnpublishedReadsOnlyItsEvents(t *testing.T) {
	a := trainSetup(t)
	before := floodEvents(a, floodSize)
	a.alarmUnpublished([]string{"t1"}, nil)
	a.alarmUnpublished([]string{"t1"}, nil)
	readFew(t, a, before, "alarmUnpublished")
	count := func() int {
		es, err := a.Store.TaskEvents("t1", EventLandedNoPR)
		must(t, err)
		return len(es)
	}
	if n := count(); n != 1 {
		t.Fatalf("alarmed %d times, want once", n)
	}
	a.Store.Event("t1", "landed", "x")
	a.alarmUnpublished([]string{"t1"}, nil)
	if n := count(); n != 2 {
		t.Fatalf("after a fresh landing alarmed %d times, want 2", n)
	}
}

// #778 follow-up: rebuilding a lost train note from the landings, in
// accountLandings and landedAll, reads only landed events.
func TestLandingRebuildReadsOnlyLandedEvents(t *testing.T) {
	a := trainSetup(t)
	queueTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	queueTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	rs, err := a.Land()
	must(t, err)
	want := trainNote(t, a, "t2")
	_, to, _ := strings.Cut(want, "..")
	before := floodEvents(a, floodSize)

	must(t, a.Store.SetTrain("t2", store.TrainOK, to, false))
	a.accountLandings(nil, rs)
	if got := trainNote(t, a, "t2"); got != want {
		t.Fatalf("accountLandings: t2 note = %q, want %q", got, want)
	}
	readFew(t, a, before, "accountLandings")

	must(t, a.Store.SetTrain("t2", store.TrainOK, to, false))
	before = a.Store.EventRowsRead()
	all, err := a.landedAll()
	must(t, err)
	from, _, _ := strings.Cut(want, "..")
	var got *landedTask
	for i := range all {
		if all[i].ID == "t2" {
			got = &all[i]
		}
	}
	if got == nil || !got.Recovered || got.To != to || got.From != from {
		t.Fatalf("landedAll t2 = %+v, want recovered %s", got, want)
	}
	readFew(t, a, before, "landedAll")
}
