package app

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// routedAround builds a red t1 with t2 building on it (same file) and runs
// the stall budget past the stuck-stack card, so t1 is routed around and
// t2 is route-around held on it. It returns the app and the time of the
// route.
func routedAround(t *testing.T) (*App, time.Time) {
	t.Helper()
	a, _, now := redStack(t, [2]string{"t2", "one.txt"})
	if _, err := a.CheckStuck(now); err != nil {
		t.Fatal(err)
	}
	at := now.Add(31 * time.Minute)
	if _, err := a.StallTick(at); err != nil {
		t.Fatal(err)
	}
	held, err := a.StallHeld()
	must(t, err)
	if len(held) != 1 || held[0].Red != "t1" || held[0].PR == "" || !slices.Equal(held[0].Held, []string{"t2"}) {
		t.Fatalf("held = %+v, want t2 held on t1's standalone PR", held)
	}
	if got := trainStateOf(t, a, "t2"); got != TrainSuperseded {
		t.Fatalf("t2's train row = %q, want held off the stack", got)
	}
	_, _ = a.Store.TakeNotices(OrchestratorID, false)
	return a, at
}

// assertReleased checks the route on t1 is gone, t2 is back in the train
// and t1 isn't, and the orchestrator heard about it exactly once.
func assertReleased(t *testing.T, a *App, at time.Time, why string) {
	t.Helper()
	if held, _ := a.StallHeld(); len(held) != 0 {
		t.Fatalf("route still held: %+v", held)
	}
	if got := trainStateOf(t, a, "t2"); got == TrainSuperseded {
		t.Fatalf("t2 wasn't requeued once t1 %s", why)
	}
	if got := trainStateOf(t, a, "t1"); got != TrainSuperseded {
		t.Fatalf("t1's train row = %q; a red layer that %s must not come back", got, why)
	}
	count := func() int {
		es, err := a.Store.Events(1000)
		must(t, err)
		n := 0
		for _, e := range es {
			if (e.Kind == EventNoticeDigest || e.Kind == EventNoticeInterrupt) && strings.Contains(e.Data, "t2") && strings.Contains(e.Data, why) {
				n++
			}
		}
		return n
	}
	if n := count(); n != 1 {
		t.Fatalf("release notices = %d, want 1", n)
	}
	if _, err := a.StallTick(at.Add(2 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("release notices = %d after another tick, want still 1", n)
	}
}

// #776: a red layer whose PR is closed never merges, so the layers held on
// it go back in the train on base without its work instead of waiting
// forever.
func TestStallRouteReleasedWhenRedPRClosed(t *testing.T) {
	t.Parallel()
	a, at := routedAround(t)
	setPR(t, mustTask(t, a, "t1").PR, "CLOSED", a.Cfg.Base)

	if _, err := a.StallTick(at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertReleased(t, a, at, "was closed")
}

// #776: the same when the red task is killed.
func TestStallRouteReleasedWhenRedKilled(t *testing.T) {
	t.Parallel()
	a, at := routedAround(t)
	must(t, a.Store.SetStatus("t1", store.Killed))

	if _, err := a.StallTick(at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	assertReleased(t, a, at, "was killed")
}

// #776: a route that resolves no way within a day is dropped with one
// blocker issue, so a route never sits in stall-held.json forever.
func TestStallRouteExpiresAfterMaxAge(t *testing.T) {
	t.Parallel()
	a, at := routedAround(t)
	if _, err := a.StallTick(at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if held, _ := a.StallHeld(); len(held) != 1 {
		t.Fatalf("route dropped early: %+v", held)
	}

	if _, err := a.StallTick(at.Add(stallRouteMaxAge + time.Minute)); err != nil {
		t.Fatal(err)
	}
	if held, _ := a.StallHeld(); len(held) != 0 {
		t.Fatalf("route still held past its max age: %+v", held)
	}
	es, err := a.Store.Events(400)
	must(t, err)
	var expired []store.Event
	for _, e := range es {
		if e.Kind == eventStallRouteExpired {
			expired = append(expired, e)
		}
	}
	if len(expired) != 1 || expired[0].Task != "t1" {
		t.Fatalf("expired events = %+v, want one for t1", expired)
	}
	b, ok := a.blockerFromEvent(expired[0])
	if !ok || b.Kind != BlockerRouteExpired || !strings.Contains(b.Detail, "t2") {
		t.Fatalf("blocker = %+v, %v; want a route-expired blocker naming t2", b, ok)
	}
	if !slices.ContainsFunc(noticesFor(t, a, OrchestratorID), func(n store.Notice) bool {
		return strings.Contains(n.Text, "t1") && strings.Contains(n.Text, "saddle requeue t2")
	}) {
		t.Fatal("orchestrator wasn't told how to put t2 back")
	}
	if _, err := a.StallTick(at.Add(stallRouteMaxAge + 2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	es, _ = a.Store.Events(400)
	n := 0
	for _, e := range es {
		if e.Kind == eventStallRouteExpired {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expired events = %d after another tick, want 1", n)
	}
}
