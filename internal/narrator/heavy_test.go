package narrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

type fakeHeavy struct {
	leases []HeavyLease
	err    error
	reads  int
}

func (f *fakeHeavy) HeavyLeases() ([]HeavyLease, error) {
	f.reads++
	return f.leases, f.err
}

func heavyNarrator(h *fakeHeavy) (*Narrator, *fakeSource, *fakeClock, *sink, *fakeDoer) {
	src, clk, out, doer := &fakeSource{}, &fakeClock{t: time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)}, &sink{}, &fakeDoer{}
	deps := Deps{Source: src, Roster: roster, Clock: clk, HTTP: doer, Sink: out}
	if h != nil {
		deps.Heavy = h
	}
	return New(Config{APIKey: "k"}, deps), src, clk, out, doer
}

func step(t *testing.T, n *Narrator) {
	t.Helper()
	if err := n.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func texts(ls []Line) []string {
	var out []string
	for _, l := range ls {
		out = append(out, l.String())
	}
	return out
}

// TestNarratorMentionsLongWaitAndOverdueOnce: a wait past 5 minutes and a
// holder past its max_run get one local line each, however many ticks they
// last, and no API call (#242).
func TestNarratorMentionsLongWaitAndOverdueOnce(t *testing.T) {
	h := &fakeHeavy{leases: []HeavyLease{
		{Lease: "aaaa1111bbbb", Class: "go-test", Who: "t1", Cmd: "make check", Holder: true, Age: 20 * time.Minute, MaxRun: 30 * time.Minute},
		{Lease: "cccc2222dddd", Class: "go-test", Who: "t2", Cmd: "go test ./...", Position: 1, Age: 4 * time.Minute, Holders: "t1"},
	}}
	n, _, clk, out, doer := heavyNarrator(h)
	step(t, n)
	if len(out.lines) != 0 {
		t.Fatalf("nothing is late yet: %q", texts(out.lines))
	}
	h.leases[0].Age, h.leases[0].Overdue = 31*time.Minute, true
	h.leases[1].Age = 6 * time.Minute
	for range 5 {
		clk.advance(time.Minute)
		step(t, n)
	}
	got := texts(out.lines)
	if len(got) != 2 {
		t.Fatalf("want one line each, got %q", got)
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{
		"t1: go-test run (make check) is overdue: 31m, past max_run 30m; saddle runq kill aaaa1111 stops it",
		"t2: has waited 6m for a go-test slot (position 1, behind t1)",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("lines lack %q:\n%s", want, all)
		}
	}
	if len(doer.reqs) != 0 {
		t.Fatalf("heavy-run lines should not call the API: %d calls", len(doer.reqs))
	}
	// A new lease past the threshold is a new mention.
	h.leases = append(h.leases, HeavyLease{Lease: "eeee3333ffff", Class: "e2e", Who: "other/t9", Cmd: "make e2e", Position: 1, Age: 7 * time.Minute})
	clk.advance(time.Minute)
	step(t, n)
	if len(out.lines) != 3 || !strings.HasPrefix(out.lines[2].String(), "other/t9: has waited 7m for an e2e slot") {
		t.Fatalf("lines %q", texts(out.lines))
	}
}

// TestNarratorBackpressureOnce: spawn refusals because the queue is backed
// up get one local line, not one per refusal, and stay out of the model's
// batch.
func TestNarratorBackpressureOnce(t *testing.T) {
	n, src, clk, out, doer := heavyNarrator(nil)
	src.push(
		store.Event{Task: "t0", Kind: "runq_backpressure", Data: "go-test has 3 waiting"},
		store.Event{Task: "t0", Kind: "runq_backpressure", Data: "go-test has 3 waiting"},
	)
	step(t, n)
	src.push(store.Event{Task: "t0", Kind: "runq_backpressure", Data: "go-test has 4 waiting"})
	clk.advance(time.Minute)
	step(t, n)
	got := texts(out.lines)
	if len(got) != 1 || got[0] != "t0: spawn refused: the heavy-run queue is backed up (go-test has 3 waiting)" {
		t.Fatalf("lines %q", got)
	}
	clk.advance(time.Minute)
	step(t, n)
	if len(doer.reqs) != 0 || n.Pending() != 0 {
		t.Fatalf("backpressure reached the model: %d calls, %d pending", len(doer.reqs), n.Pending())
	}
	// Once the quiet spell passes, a new refusal is news again.
	clk.advance(backpressureQuiet)
	src.push(store.Event{Task: "t0", Kind: "runq_backpressure", Data: "e2e has 2 waiting"})
	step(t, n)
	if len(out.lines) != 2 {
		t.Fatalf("lines %q", texts(out.lines))
	}
}

// TestNarratorHeavyErrorKeepsEvents: a queue that can't be read is
// reported, and the events polled in the same step are still narrated.
func TestNarratorHeavyErrorKeepsEvents(t *testing.T) {
	h := &fakeHeavy{err: errors.New("database is locked")}
	n, src, _, _, _ := heavyNarrator(h)
	src.push(store.Event{Task: "t1", Kind: "tool_use"})
	if err := n.Step(context.Background()); err == nil || !strings.Contains(err.Error(), "database is locked") {
		t.Fatalf("step error %v", err)
	}
	if n.Pending() != 1 {
		t.Fatalf("the polled event was dropped: %d pending", n.Pending())
	}
}
