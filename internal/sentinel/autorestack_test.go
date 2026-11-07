package sentinel

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/gitx"
)

// #775: t1 leaves the stack (superseded) while its commits are still on
// integration, so t2 reads "contains 1 commit from other tasks". A restack
// fixes that without a human, so the sentinel runs it itself and never flags.
func TestSentinelRestacksWhenOnlyDriftAfterLayerLeaves(t *testing.T) {
	a, _ := setup(t)
	newFakeGH(t)
	t1 := landTask(t, a, "t1", "one")
	landTask(t, a, "t2", "two")
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Unstack(t1.ID); err != nil {
		t.Fatal(err)
	}
	rep, err := New(a).Check()
	must(t, err)
	if rep.AtRisk || !rep.Restacked {
		t.Fatalf("report = %+v, want a restack and a clean stack", rep)
	}
	if _, flagged, _ := a.Flag(); flagged {
		t.Fatal("stack flagged for drift a restack fixed")
	}
	if len(events(t, a, EventAtRisk)) != 0 {
		t.Fatalf("stack_at_risk recorded: %+v", events(t, a, EventAtRisk))
	}
	if len(events(t, a, EventRestack)) != 1 {
		t.Fatalf("sentinel_restack events = %+v", events(t, a, EventRestack))
	}
	if _, err := a.PRs(); err != nil {
		t.Fatalf("prs after the sentinel's restack: %v", err)
	}
}

// Main moves under the stack: the sentinel restacks onto it instead of
// flagging "origin/main has 1 commit saddle/integration lacks".
func TestSentinelRestacksWhenBaseMoves(t *testing.T) {
	a, origin := setup(t)
	newFakeGH(t)
	landTask(t, a, "t1", "one")
	landTask(t, a, "t2", "two")
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	write(t, other, "main.txt", "main\n")
	commitAll(t, other, "main moves")
	git(t, other, "push", "-q", "origin", "main")

	rep, err := New(a).Check()
	must(t, err)
	if rep.AtRisk || !rep.Restacked {
		t.Fatalf("report = %+v, want a restack and a clean stack", rep)
	}
	head := git(t, other, "rev-parse", "HEAD")
	if _, err := gitx.Run(a.Root, "merge-base", "--is-ancestor", head, a.Cfg.Integration); err != nil {
		t.Fatalf("integration not rebuilt on the new main: %v", err)
	}
	if _, flagged, _ := a.Flag(); flagged {
		t.Fatal("stack flagged for a moved base")
	}
}

// driftStack lands t1 and t2 and moves main, so the only finding is drift.
func driftStack(t *testing.T) *app.App {
	t.Helper()
	a, origin := setup(t)
	newFakeGH(t)
	landTask(t, a, "t1", "one")
	landTask(t, a, "t2", "two")
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	write(t, other, "main.txt", "main\n")
	commitAll(t, other, "main moves")
	git(t, other, "push", "-q", "origin", "main")
	return a
}

// A restack that fails escalates: the flag goes up naming the failure. The
// sentinel tries again only after a backoff, and at most three times per
// flag.
func TestSentinelEscalatesFailedRestackWithBackoff(t *testing.T) {
	a := driftStack(t)
	s := New(a)
	now := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	calls := 0
	s.restack = func() (app.RestackResult, error) {
		calls++
		return app.RestackResult{}, errors.New("restack can't rebuild t1: boom")
	}
	rep, err := s.Check()
	must(t, err)
	if calls != 1 || !rep.AtRisk || !strings.Contains(rep.Cause, "boom") {
		t.Fatalf("after a failed restack: calls %d, report %+v", calls, rep)
	}
	if _, flagged, _ := a.Flag(); !flagged {
		t.Fatal("a failed restack did not raise the flag")
	}
	if evs := events(t, a, EventAtRisk); len(evs) != 1 {
		t.Fatalf("stack_at_risk events = %+v", evs)
	}
	// Backing off: the next cycle doesn't restack again yet.
	if _, err := s.Check(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("restacked again inside the backoff: %d calls", calls)
	}
	for i := 0; i < 5; i++ {
		now = now.Add(time.Hour)
		if _, err := s.Check(); err != nil {
			t.Fatal(err)
		}
	}
	if calls != maxRestacks {
		t.Fatalf("restack ran %d times, want the cap of %d", calls, maxRestacks)
	}
	if _, flagged, _ := a.Flag(); !flagged {
		t.Fatal("flag lifted though restack never worked")
	}
}

// A restack that races automerge's own push fails with "stale info". The
// sentinel fetches and retries once; if that races too, it waits for the next
// cycle instead of flagging.
func TestSentinelRetriesStaleInfoRestackOnce(t *testing.T) {
	a := driftStack(t)
	s := New(a)
	calls := 0
	stale := errors.New("! [rejected] saddle/integration (stale info)")
	s.restack = func() (app.RestackResult, error) {
		calls++
		if calls == 1 {
			return app.RestackResult{}, stale
		}
		return a.Restack()
	}
	rep, err := s.Check()
	must(t, err)
	if calls != 2 || rep.AtRisk || !rep.Restacked {
		t.Fatalf("calls %d, report %+v; want one retry that fixes the stack", calls, rep)
	}
	if _, flagged, _ := a.Flag(); flagged {
		t.Fatal("flagged after the retried restack worked")
	}
}

func TestSentinelWaitsNextCycleWhenStaleInfoPersists(t *testing.T) {
	a := driftStack(t)
	s := New(a)
	calls := 0
	s.restack = func() (app.RestackResult, error) {
		calls++
		return app.RestackResult{}, errors.New("! [rejected] saddle/t1 (stale info)")
	}
	rep, err := s.Check()
	must(t, err)
	if calls != 2 {
		t.Fatalf("restack ran %d times, want one retry", calls)
	}
	if rep.AtRisk || !rep.Deferred {
		t.Fatalf("report = %+v, want the restack left for the next cycle", rep)
	}
	if _, flagged, _ := a.Flag(); flagged {
		t.Fatal("flagged for a push race")
	}
	if len(events(t, a, EventAtRisk)) != 0 {
		t.Fatal("stack_at_risk recorded for a push race")
	}
	// The next cycle tries again, and a real restack fixes it.
	s.restack = a.Restack
	rep, err = s.Check()
	must(t, err)
	if rep.AtRisk || !rep.Restacked {
		t.Fatalf("next cycle: %+v", rep)
	}
}

// A conflict needs a human: the sentinel never restacks over one.
func TestSentinelDoesNotRestackConflict(t *testing.T) {
	a, _ := setup(t)
	gh := newFakeGH(t)
	t1 := landTask(t, a, "t1", "one")
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	t1, _ = a.Store.Task(t1.ID)
	gh.setPR(t1.PR, "OPEN", "CONFLICTING")
	s := New(a)
	s.restack = func() (app.RestackResult, error) {
		t.Fatal("restacked over a conflict")
		return app.RestackResult{}, nil
	}
	rep, err := s.Check()
	must(t, err)
	if !rep.AtRisk {
		t.Fatalf("report = %+v, want the conflict flagged", rep)
	}
}
