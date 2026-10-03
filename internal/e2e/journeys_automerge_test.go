//go:build e2e

package e2e

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/e2e/fakegh"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// landThree spawns three agents on disjoint claims, lands them and opens
// their PRs. With the default "stack" layout the tasks are unrelated, so
// each PR is its own stack on main. It returns the PR URLs by task.
func landThree(t *testing.T, w *World) map[string]string {
	t.Helper()
	dirs := map[string]string{"t1": "alpha", "t2": "beta", "t3": "gamma"}
	for _, id := range []string{"t1", "t2", "t3"} {
		w.Spawn(id, dirs[id]+" work", []string{dirs[id] + "/**"}, finished(dirs[id], dirs[id]+"\n")...)
	}
	for _, id := range []string{"t1", "t2", "t3"} {
		w.WaitTask(id, "queued in the train", func(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" })
	}
	w.MustSaddle("land")
	w.MustSaddle("prs")
	urls := map[string]string{}
	s := w.GHState()
	for _, id := range []string{"t1", "t2", "t3"} {
		urls[id] = w.Task(id).PR
		if urls[id] == "" {
			t.Fatalf("%s has no PR", id)
		}
		if p := prNumber(t, s, urls[id]); p.Base != "main" {
			t.Fatalf("%s's PR targets %s, want main (three separate stacks)", id, p.Base)
		}
	}
	return urls
}

// events lists the events of kind since the World's repo was made.
func events(t *testing.T, a *app.App, kind string) []string {
	t.Helper()
	evs, err := a.Store.EventsSince(0)
	must(t, err)
	var out []string
	for _, e := range evs {
		if e.Kind == kind {
			out = append(out, e.Task+": "+e.Data)
		}
	}
	return out
}

// TestJourneyAutomergeSkipsRedStackAndMergesReadyOne: three stacks on main,
// the first in train order with red CI. Auto-merge refuses the red one and
// merges the two green ones, one per tick, instead of waiting behind it.
func TestJourneyAutomergeSkipsRedStackAndMergesReadyOne(t *testing.T) {
	w := world(t, Options{})
	urls := landThree(t, w)
	must(t, w.GH.SetAllChecks(fakegh.Pass))
	s := w.GHState()
	must(t, w.GH.SetChecks(prNumber(t, s, urls["t1"]).Number, fakegh.Check{Name: "ci", Workflow: "CI", State: fakegh.Fail}))
	w.MustSaddle("automerge", "on")

	st := w.AutomergeTick()
	if len(st.Stacks) != 3 || st.Stacks[0].ID != "t1" || st.Stacks[0].Ready || !strings.Contains(st.Stacks[0].Why, "red") {
		t.Fatalf("want t1 first and refused for red CI: %+v", st.Stacks)
	}
	if st.Merged != urls["t2"] {
		t.Fatalf("first tick merged %q, want t2's %s past the red t1: %+v", st.Merged, urls["t2"], st)
	}
	st = w.AutomergeTick()
	if st.Merged != urls["t3"] {
		t.Fatalf("second tick merged %q, want t3's %s: %+v", st.Merged, urls["t3"], st)
	}
	s = w.GHState()
	if p := prNumber(t, s, urls["t1"]); p.State != "OPEN" {
		t.Fatalf("red t1 = %s, want still open", p.State)
	}
	a := w.App()
	if refused := events(t, a, automerge.EventRefused); len(refused) != 1 || !strings.Contains(refused[0], "red") {
		t.Fatalf("refusals = %q, want one for t1's red CI", refused)
	}
	out := w.MustSaddle("automerge", "status").Stdout
	if !strings.Contains(out, "stack t1") || !strings.Contains(out, "its CI is red") {
		t.Fatalf("status doesn't say why t1 waits:\n%s", out)
	}
}

// TestJourneyAutomergeRetriesSoonAfterBusyTrain reproduces 2026-10-03: two
// ready PRs sat for over an hour because every tick of the watcher found
// the train lock held (a land's test gate, a restack, the sentinel) and
// skipped silently until the next two-minute tick, with nothing saved or
// logged. Now a busy tick says so in status, logs that a ready PR wasn't
// merged and why, and retries within seconds, so the PR merges as soon as
// the lock frees, long before the next regular tick.
func TestJourneyAutomergeRetriesSoonAfterBusyTrain(t *testing.T) {
	w := world(t, Options{})
	w.Spawn("t1", "alpha work", []string{"alpha/**"}, finished("alpha", "alpha\n")...)
	w.WaitTask("t1", "queued in the train", func(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" })
	w.MustSaddle("land")
	w.MustSaddle("prs")
	url := w.Task("t1").PR
	must(t, w.GH.SetAllChecks(fakegh.Pass))
	w.MustSaddle("automerge", "on")

	a := w.App()
	unlock, ok, err := a.TryLockTrain()
	must(t, err)
	if !ok {
		t.Fatal("train lock already held")
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()

	// The regular interval is an hour: only a quick busy retry can merge it
	// within the test's timeout.
	wa := w.App()
	wt := wa.NewAutomerge(nil)
	wt.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- wt.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("run: %v", err)
		}
	}()

	Eventually(t, "a busy tick to say why the ready PR isn't merging", func() error {
		idle := events(t, a, "automerge_idle")
		if len(idle) == 0 || !strings.Contains(idle[0], url) || !strings.Contains(idle[0], "train lock") {
			return errorf("idle events = %q", idle)
		}
		return nil
	})
	out := w.MustSaddle("automerge", "status").Stdout
	for _, want := range []string{"ready to merge", "train lock", "next check"} {
		if !strings.Contains(out, want) {
			t.Fatalf("status lacks %q:\n%s", want, out)
		}
	}
	if p := prNumber(t, w.GHState(), url); p.State != "OPEN" {
		t.Fatalf("merged while the train was busy: %+v", p)
	}

	unlock()
	locked = false
	Eventually(t, "the PR to merge once the train lock frees", func() error {
		if p := prNumber(t, w.GHState(), url); p.State != "MERGED" {
			return errorf("PR is %s", p.State)
		}
		if merged := events(t, a, automerge.EventMerged); len(merged) != 1 {
			return errorf("merged events = %q", merged)
		}
		return nil
	})
}
