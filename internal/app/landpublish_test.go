package app

import (
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

func prCreates(log []string) int {
	n := 0
	for _, l := range log {
		if strings.HasPrefix(l, "pr create") {
			n++
		}
	}
	return n
}

// #819: seven tasks were reported landed and done, but land never opened
// their PRs and nobody ran prs, so none reached main. Land now opens the PR
// in the same step, and automerge carries it to base without a prs call.
func TestLandOpensPRAndAutomergeCarriesItToBase(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, calls := originWithGh(t, a)
	queueTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})

	rs, err := a.LandRecorded()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TrainOK {
		t.Fatalf("land = %+v", rs)
	}
	t1, err := a.Store.Task("t1")
	must(t, err)
	if t1.PR == "" || prCreates(calls()) != 1 {
		t.Fatalf("land left t1 with no PR (pr %q, gh calls %q)", t1.PR, calls())
	}
	if !strings.Contains(rs[0].Note, t1.PR) || !strings.Contains(rs[0].Note, DeliveryPROpen) {
		t.Fatalf("land result note %q doesn't name the PR it opened", rs[0].Note)
	}
	if got := Delivery(t1.Status, store.TrainOK, t1.PR); got != DeliveryPROpen {
		t.Fatalf("delivery = %q, want %q", got, DeliveryPROpen)
	}

	gh := newSquashGH(t, a, origin)
	w := a.NewAutomerge(gh)
	must(t, w.SetEnabled(true))
	for range 3 {
		_, err := w.Check()
		must(t, err)
	}
	if !slices.Equal(gh.order, []string{"t1"}) {
		t.Fatalf("automerge merged %v, want t1", gh.order)
	}
	if got := trainState(t, a, "t1"); got != TrainMerged {
		t.Fatalf("t1 train state = %q after its PR merged, want merged", got)
	}
}

// #819 migration: a repo where an older saddle landed N tasks without PRs
// gets all N opened on the first run, by land or the sentinel.
func TestPublishLandedOpensPRsForLandedTasksWithoutOne(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	_, calls := originWithGh(t, a)
	for i, id := range []string{"t1", "t2", "t3"} {
		landTask(t, a, id, id, map[string]string{id + ".txt": strings.Repeat("x", i+1) + "\n"}) // the old land: no PR
	}
	if n := prCreates(calls()); n != 0 {
		t.Fatalf("raw land opened %d PRs", n)
	}
	un, err := a.Unpublished()
	must(t, err)
	if !slices.Equal(un, []string{"t1", "t2", "t3"}) {
		t.Fatalf("unpublished = %v", un)
	}

	opened, err := a.PublishLanded("migration")
	must(t, err)
	if !slices.Equal(opened, []string{"t1", "t2", "t3"}) || prCreates(calls()) != 3 {
		t.Fatalf("opened %v with %d pr creates, want t1..t3 and 3", opened, prCreates(calls()))
	}
	if un, _ := a.Unpublished(); len(un) != 0 {
		t.Fatalf("still unpublished: %v", un)
	}
	// A second run has nothing to do.
	opened, err = a.PublishLanded("again")
	must(t, err)
	if len(opened) != 0 || prCreates(calls()) != 3 {
		t.Fatalf("second run opened %v", opened)
	}
}

// #819: a landed task whose PR can't open is an alarm, raised once, and the
// next run heals it.
func TestLandedTaskWithNoPRIsAlarmedThenHealed(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	_, calls := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	// The stack is flagged at t1: prs publishes nothing from there up.
	must(t, a.SetFlag(StackFlag{Task: "t1", Cause: "test"}))

	_, err := a.PublishLanded(PublishBySentinel)
	if err == nil {
		t.Fatal("publish under a flag reported no error")
	}
	_, _ = a.PublishLanded(PublishBySentinel) // a second cycle doesn't alarm twice
	if n := landedNoPRAlarms(t, a, "t1", EventLandedNoPR); n != 1 {
		t.Fatalf("%d landed_no_pr alarms, want 1", n)
	}
	ns, err := a.Store.PeekNotices(OrchestratorID, false)
	must(t, err)
	if !slices.ContainsFunc(ns, func(n store.Notice) bool {
		return strings.Contains(n.Text, "t1 landed locally but has no PR") && strings.Contains(n.Text, "not done")
	}) {
		t.Fatalf("orchestrator not told: %+v", ns)
	}

	must(t, a.ClearFlag())
	opened, err := a.PublishLanded(PublishBySentinel)
	must(t, err)
	if !slices.Equal(opened, []string{"t1"}) || prCreates(calls()) != 1 {
		t.Fatalf("heal opened %v", opened)
	}
}

// #819: a repo with no remote has nowhere to publish; nothing happens.
func TestPublishLandedNeedsARemote(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	opened, err := a.PublishLanded("land")
	if err != nil || len(opened) != 0 {
		t.Fatalf("opened %v, err %v", opened, err)
	}
}

// #819: no wording ever calls a landed, unmerged task done or merged.
func TestDeliveryNeverCallsUnmergedWorkDone(t *testing.T) {
	t.Parallel()
	cases := []struct{ status, train, pr, want string }{
		{store.Landed, store.TrainOK, "", DeliveryNoPR},
		{store.Landed, store.TrainOK, "https://github.com/o/r/pull/1", DeliveryPROpen},
		{store.Killed, store.TrainOK, "", DeliveryNoPR},
		{store.Landed, TrainMerged, "https://github.com/o/r/pull/1", DeliveryMerged},
		{store.Running, "", "", ""},
		{store.Done, store.Queued, "", ""},
	}
	for _, c := range cases {
		got := Delivery(c.status, c.train, c.pr)
		if got != c.want {
			t.Errorf("Delivery(%q, %q, %q) = %q, want %q", c.status, c.train, c.pr, got, c.want)
		}
		if c.train != TrainMerged && (strings.Contains(got, "merged") || strings.Contains(got, "done")) {
			t.Errorf("Delivery(%q, %q, %q) = %q calls unmerged work done", c.status, c.train, c.pr, got)
		}
	}
}

func landedNoPRAlarms(t *testing.T, a *App, task, kind string) int {
	t.Helper()
	evs, err := a.Store.Events(-1)
	must(t, err)
	n := 0
	for _, e := range evs {
		if e.Task == task && e.Kind == kind {
			n++
		}
	}
	return n
}

// #819: the sentinel retries a failed publish with backoff, so a red gate
// isn't rerun every cycle; land always retries.
func TestPublishLandedBacksOffSentinelRetries(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	must(t, a.SetFlag(StackFlag{Task: "t1", Cause: "test"}))
	if _, err := a.PublishLanded(PublishBySentinel); err == nil {
		t.Fatal("the first sentinel attempt under a flag reported no error")
	}
	// A second cycle right away waits: it tries nothing, so nothing fails.
	if _, err := a.PublishLanded(PublishBySentinel); err != nil {
		t.Fatalf("the sentinel retried at once: %v", err)
	}
	if _, err := a.PublishLanded("land"); err == nil {
		t.Fatal("land waited for the sentinel's backoff")
	}
	must(t, a.ClearFlag())
	opened, err := a.PublishLanded("land")
	must(t, err)
	if !slices.Equal(opened, []string{"t1"}) {
		t.Fatalf("land's retry opened %v", opened)
	}
}
