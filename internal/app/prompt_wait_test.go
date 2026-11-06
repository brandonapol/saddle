package app

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// #786: a worker in needs-you with no activity past hung_after gets a card
// whose default, run on its own, presses Escape and tells it the action
// isn't allowed; past its timeout it stops counting toward the cap, and a
// second timeout parks it so its slot is free.
func TestPromptWaitTimesOutNeedsYou(t *testing.T) {
	t.Parallel()
	a, ft := setup(t)
	tk := promptWorker(t, a, "t5")
	ft.setScreen(tk.Window, "Do you want to make this edit to go.mod?\n❯ 1. Yes\n  2. No")
	window := a.StallConfig().HungAfter
	now := time.Now().UTC()
	tick := func(at time.Time) {
		t.Helper()
		if _, err := a.StallTick(at); err != nil {
			t.Fatal(err)
		}
	}
	tick(now)
	tick(now.Add(window / 2))
	if cs := openCardsOf(t, a, DecisionPromptWait); len(cs) != 0 {
		t.Fatalf("carded before hung_after: %+v", cs)
	}
	if n, _ := a.activeWorkers(); n != 1 {
		t.Fatalf("active = %d before the timeout, want 1", n)
	}
	tick(now.Add(window))
	cs := openCardsOf(t, a, DecisionPromptWait)
	if len(cs) != 1 || cs[0].DefaultOption().Run != RunDenyPrompt {
		t.Fatalf("cards after hung_after = %+v, want one that denies by default", cs)
	}
	if n, _ := a.activeWorkers(); n != 0 {
		t.Fatalf("active = %d past the timeout, want 0: it still holds a slot", n)
	}
	tick(now.Add(window + cs[0].Timeout + time.Minute))
	if got := keysSent(t, a, "t5"); !slices.Equal(got, []string{"Escape"}) {
		t.Fatalf("keys = %q, want Escape from the card's default", got)
	}
	if ns := noticesFor(t, a, "t5"); !slices.ContainsFunc(ns, func(n store.Notice) bool { return strings.Contains(n.Text, "isn't allowed here") }) {
		t.Fatalf("t5 wasn't told: %+v", ns)
	}
	if st := mustTask(t, a, "t5").Status; !liveStatus(st) {
		t.Fatalf("status = %q after the first timeout, want it still working", st)
	}
	// Still stuck on the prompt: the next timeout reopens the card, and its
	// default parks the task this time.
	later := now.Add(window + cs[0].Timeout + time.Minute)
	tick(later.Add(window))
	if cs := openCardsOf(t, a, DecisionPromptWait); len(cs) != 1 {
		t.Fatalf("no second card: %+v", cs)
	}
	tick(later.Add(2*window + time.Minute))
	if st := mustTask(t, a, "t5").Status; st != StatusPaused {
		t.Fatalf("status = %q after the second timeout, want paused", st)
	}
	if ft.windows[tk.Window] {
		t.Fatal("window still alive: the slot isn't free")
	}
	autoExit(t, a, "t5")
}

// A needs-you worker that keeps moving is not timed out, and one that
// leaves needs-you closes its card.
func TestPromptWaitResetsOnProgressAndExit(t *testing.T) {
	t.Parallel()
	a, ft := setup(t)
	tk := promptWorker(t, a, "t5")
	window := a.StallConfig().HungAfter
	now := time.Now().UTC()
	for i := range 4 {
		ft.setScreen(tk.Window, strings.Repeat("thinking more ", i+1))
		if _, err := a.PromptWaitTick(now.Add(time.Duration(i) * window / 2)); err != nil {
			t.Fatal(err)
		}
	}
	if cs := openCardsOf(t, a, DecisionPromptWait); len(cs) != 0 {
		t.Fatalf("carded a worker that moves: %+v", cs)
	}
	at := now.Add(2 * window)
	if _, err := a.PromptWaitTick(at.Add(window)); err != nil {
		t.Fatal(err)
	}
	if cs := openCardsOf(t, a, DecisionPromptWait); len(cs) != 1 {
		t.Fatalf("cards = %+v, want one", cs)
	}
	must(t, a.Store.SetStatus("t5", store.Running))
	if _, err := a.PromptWaitTick(at.Add(window + time.Minute)); err != nil {
		t.Fatal(err)
	}
	if cs := openCardsOf(t, a, DecisionPromptWait); len(cs) != 0 {
		t.Fatalf("card still open after it left needs-you: %+v", cs)
	}
	if a.PromptOverdue()["t5"] {
		t.Fatal("still overdue after it left needs-you")
	}
}
