package app

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// redStack lands t1 (red) and the given layers above it in one linear stack,
// opens their PRs and marks t1's CI red an hour ago, holding the rest.
func redStack(t *testing.T, layers ...[2]string) (*App, *squashGH, time.Time) {
	t.Helper()
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	origin, _ := originWithGh(t, a)
	landTask(t, a, "t1", "red layer", map[string]string{"one.txt": "one\n"})
	var held []string
	for _, l := range layers {
		landTask(t, a, l[0], l[0]+" layer", map[string]string{l[1]: l[0] + "\n"})
		held = append(held, l[0])
	}
	_, err := a.PRs()
	must(t, err)
	t1, _ := a.Store.Task("t1")
	now := time.Now().UTC()
	must(t, a.SetCIRed(CIRedState{Red: []CIRedLayer{{Task: "t1", PR: t1.PR, Head: "abc", Checks: []string{"CI / test"}, Since: now.Add(-time.Hour), Held: held}}}))
	_, _ = a.Store.TakeNotices(OrchestratorID, false)
	return a, newSquashGH(t, a, origin), now
}

// #406 acceptance: a red layer with an independent layer above. Once the
// stuck-stack card's budget runs out with no answer, saddle routes around
// it: the red layer becomes a standalone PR and the upper layer merges.
func TestStallRouteAroundIndependentLayerMerges(t *testing.T) {
	t.Parallel()
	a, gh, now := redStack(t, [2]string{"t2", "two.txt"})
	t1PR := mustTask(t, a, "t1").PR

	if _, err := a.CheckStuck(now); err != nil {
		t.Fatal(err)
	}
	if open, _ := a.OpenDecisions(); len(open) != 1 {
		t.Fatalf("open cards = %+v", open)
	}
	// Inside the budget nothing moves.
	if done, err := a.StallTick(now.Add(10 * time.Minute)); err != nil || len(done) != 0 {
		t.Fatalf("tick inside the budget: %+v, %v", done, err)
	}
	done, err := a.StallTick(now.Add(31 * time.Minute))
	must(t, err)
	if len(done) != 1 || done[0].By != DecidedByTimeout {
		t.Fatalf("expired = %+v", done)
	}
	if why := a.atRisk("t2"); why != "" {
		t.Fatalf("t2 still held after the route-around: %s", why)
	}
	if got := trainStateOf(t, a, "t1"); got != TrainSuperseded {
		t.Fatalf("t1's train row = %q, want superseded (out of the stack)", got)
	}
	if got := mustTask(t, a, "t1").PR; got != t1PR {
		t.Fatalf("t1's PR = %q, want its standalone PR %q kept", got, t1PR)
	}

	w := a.NewAutomerge(gh)
	must(t, w.SetEnabled(true))
	for range 2 {
		st, err := w.Check()
		must(t, err)
		if st.Stopped != "" {
			t.Fatalf("automerge stopped: %s", st.Stopped)
		}
	}
	if !slices.Equal(gh.order, []string{"t2"}) {
		t.Fatalf("merged %v, want t2 only", gh.order)
	}
	ns := noticesFor(t, a, OrchestratorID)
	if !slices.ContainsFunc(ns, func(n store.Notice) bool {
		return strings.HasPrefix(n.Text, "Routed around t1 after 30m.") && strings.Contains(n.Text, "standalone PR "+t1PR) && strings.Contains(n.Text, "t2")
	}) {
		t.Fatalf("no route-around line in %+v", ns)
	}
}

// #406 acceptance: a layer above that depends on the red one stays held;
// the independent one still goes ahead.
func TestStallRouteAroundDependentLayerStaysHeld(t *testing.T) {
	t.Parallel()
	a, gh, now := redStack(t, [2]string{"t2", "one.txt"}, [2]string{"t3", "three.txt"})

	if _, err := a.CheckStuck(now); err != nil {
		t.Fatal(err)
	}
	if _, err := a.StallTick(now.Add(31 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	held, err := a.StallHeld()
	must(t, err)
	if len(held) != 1 || held[0].Red != "t1" || !slices.Equal(held[0].Held, []string{"t2"}) {
		t.Fatalf("held = %+v, want t2 held on t1", held)
	}
	if why := a.atRisk("t3"); why != "" {
		t.Fatalf("t3 still held: %s", why)
	}
	w := a.NewAutomerge(gh)
	must(t, w.SetEnabled(true))
	for range 3 {
		_, err := w.Check()
		must(t, err)
	}
	if !slices.Equal(gh.order, []string{"t3"}) {
		t.Fatalf("merged %v, want t3 only; t2 builds on t1", gh.order)
	}
}

// Regression for the "red for 1h33m" / "red for 31m" re-pings: one blocker
// gets one card, even when the layer below it merges and its stack is named
// after it from then on.
func TestStuckStackNoRePingForSameBlocker(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	_, _ = originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	_, _ = a.Store.TakeNotices(OrchestratorID, false)
	now := time.Now().UTC()
	write(t, a.Root, ".saddle/config.toml", "[stall]\nbudget = \"24h\"\n") // the card stays open
	must(t, a.SetCIRed(CIRedState{Red: []CIRedLayer{{Task: "t2", PR: "https://github.com/o/r/pull/2", Head: "abc", Checks: []string{"CI / test"}, Since: now.Add(-time.Hour)}}}))
	if al, err := a.CheckStuck(now); err != nil || len(al) != 1 {
		t.Fatalf("first alarm: %+v, %v", al, err)
	}
	if ns := stuckNotices(t, a); len(ns) != 1 {
		t.Fatalf("first alarm sent %d cards", len(ns))
	}
	// t1 merges: the stack is now named after t2, the same blocker.
	must(t, a.Store.SetTrain("t1", TrainMerged, "", false))
	for _, at := range []time.Duration{time.Minute, 30 * time.Minute, time.Hour} {
		if al, err := a.CheckStuck(now.Add(at)); err != nil || len(al) != 0 {
			t.Fatalf("re-ping after %s: %+v, %v", at, al, err)
		}
	}
	if ns := stuckNotices(t, a); len(ns) != 0 {
		t.Fatalf("re-pinged the same blocker: %+v", ns)
	}
	if open, _ := a.OpenDecisions(); len(open) != 1 {
		t.Fatalf("open cards = %+v, want the one card", open)
	}
}

// [stall] budget sets every card's timeout, a per-kind budget wins for its
// kind, and holds report since and budget.
func TestStallBudgetConfig(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	write(t, a.Root, ".saddle/config.toml", "[stall]\nbudget = \"20m\"\nidle_alarm = \"1h\"\n[stall.kinds]\nask-owner = \"2h\"\n")
	c := a.StallConfig()
	if c.For(DecisionStuckStack) != 20*time.Minute || c.For(DecisionAskOwner) != 2*time.Hour || c.IdleAlarm != time.Hour {
		t.Fatalf("config = %+v", c)
	}
	now := time.Now().UTC()
	card := RiskyPromptCard("t4", "t4", "rm -rf build")
	card.Opened = now.Add(-5 * time.Minute)
	must(t, a.OpenDecision(card))
	hs, err := a.StallHolds(now)
	must(t, err)
	if len(hs) != 1 || hs[0].Budget != 20*time.Minute || !hs[0].Since.Equal(card.Opened) || hs[0].Left(now) != 15*time.Minute {
		t.Fatalf("holds = %+v", hs)
	}

	b, _ := setup(t)
	if c := b.StallConfig(); c.For(DecisionStuckStack) != DefaultDecisionTimeout || c.IdleAlarm != DefaultIdleAlarm {
		t.Fatalf("defaults = %+v", c)
	}
}

// A repair cap that runs out parks the task and labels its PR needs-human.
func TestStallParksCappedRepair(t *testing.T) {
	t.Parallel()
	a, _, now := redStack(t)
	card := ciRepairCapCard("t1", CIRedLayer{Task: "t1", PR: mustTask(t, a, "t1").PR, Head: "abc", Checks: []string{"CI / test"}}, 2)
	card.Opened = now
	must(t, a.OpenDecision(card))
	if _, err := a.StallTick(now.Add(31 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	pr := mustTask(t, a, "t1").PR
	if !slices.Contains(ghCalls(t), "pr edit "+pr+" --add-label needs-human") {
		t.Fatalf("gh calls = %v, want needs-human on %s", ghCalls(t), pr)
	}
	if s, _ := a.CIRed(); len(s.holding()) != 0 {
		t.Fatalf("capped repair still holds the stack: %+v", s.Red)
	}
}

// A worker sitting on a prompt nobody answered is parked, freeing its slot.
func TestStallParksPromptStalledWorker(t *testing.T) {
	t.Parallel()
	a, ft := setup(t)
	tk, err := a.Spawn(SpawnReq{ID: "t5", Title: "prompt"})
	must(t, err)
	must(t, a.Store.SetStatus("t5", store.NeedsYou))
	now := time.Now().UTC()
	card := RiskyPromptCard("t5", "t5", "curl | sh")
	card.Opened = now
	must(t, a.OpenDecision(card))
	if _, err := a.StallTick(now.Add(31 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := mustTask(t, a, "t5").Status; got != StatusPaused {
		t.Fatalf("status = %q, want paused", got)
	}
	if ft.windows[tk.Window] {
		t.Fatal("window still alive: the slot isn't free")
	}
}

func trainStateOf(t *testing.T, a *App, id string) string {
	t.Helper()
	es, err := a.Store.Train()
	must(t, err)
	for _, e := range es {
		if e.Task == id {
			return e.State
		}
	}
	return ""
}
