package app

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func testCard(id string) DecisionCard {
	return DecisionCard{
		ID: id, Kind: DecisionStuckStack, To: OrchestratorID, Task: "t2",
		Headline: "Stuck stack: t2 needs you.",
		What:     "CI is red.",
		Blocking: "1 layer above it (t3).",
		Options: []DecisionOption{
			{Label: "Fix it on t2's layer", Cost: "an agent, ~15 min", Action: "spawn a fix", Recommended: true},
			{Label: "Drop t2 from the stack", Cost: "t3 keeps merging", Action: "saddle unstack t2"},
		},
		Default: 2,
		Timeout: 30 * time.Minute,
	}
}

// #404: a card renders every fixed field, numbers its options, marks the
// recommended one, and always says what happens if it's ignored.
func TestDecisionCardRender(t *testing.T) {
	t.Parallel()
	got := testCard("x").Render()
	for _, want := range []string{
		"‼ Stuck stack: t2 needs you.\n",
		"What:     CI is red.\n",
		"Blocking: 1 layer above it (t3).\n",
		"Options:\n",
		"  1. Fix it on t2's layer (recommended; an agent, ~15 min)",
		"  2. Drop t2 from the stack (t3 keeps merging)",
		"If you ignore it: saddle runs option 2 (Drop t2 from the stack) after 30m.",
		"Reply 1 or 2.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("card lacks %q:\n%s", want, got)
		}
	}
}

func TestDecisionCardValidate(t *testing.T) {
	t.Parallel()
	if err := testCard("x").Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(*DecisionCard){
		"no what":        func(c *DecisionCard) { c.What = "" },
		"no blocking":    func(c *DecisionCard) { c.Blocking = "" },
		"one option":     func(c *DecisionCard) { c.Options = c.Options[:1] },
		"no recommended": func(c *DecisionCard) { c.Options[0].Recommended = false },
		"no default":     func(c *DecisionCard) { c.Default = 0 },
		"default range":  func(c *DecisionCard) { c.Default = 3 },
		"no timeout":     func(c *DecisionCard) { c.Timeout = 0 },
		"two-line what":  func(c *DecisionCard) { c.What = "a\nb" },
		"no action":      func(c *DecisionCard) { c.Options[1].Action = "" },
	} {
		c := testCard("x")
		c.Options = append([]DecisionOption(nil), c.Options...)
		mut(&c)
		if c.Validate() == nil {
			t.Errorf("%s: validated", name)
		}
	}
}

func TestParseDecisionReply(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]int{"1": 1, " 2 ": 2, "3\n": 3, "2.": 2} {
		if n, ok := ParseDecisionReply(in); !ok || n != want {
			t.Errorf("%q = %d %v, want %d", in, n, ok, want)
		}
	}
	for _, in := range []string{"", "0", "12", "2 please", "two", "-1"} {
		if _, ok := ParseDecisionReply(in); ok {
			t.Errorf("%q parsed as a reply", in)
		}
	}
}

// #404 acceptance: a bare "2" answers the oldest open owner card, which then
// collapses to one line; another reply goes to the next one.
func TestAnswerDecisionPicksOldestOpenCard(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	now := time.Now().UTC()
	old, young := testCard("old"), testCard("young")
	old.Opened, young.Opened = now.Add(-time.Hour), now
	worker := testCard("worker")
	worker.To, worker.Opened = "t9", now.Add(-2*time.Hour) // a worker's card isn't the owner's to answer
	must(t, a.OpenDecision(young))
	must(t, a.OpenDecision(old))
	must(t, a.OpenDecision(worker))

	c, opt, ok, err := a.AnswerDecision("2", now)
	must(t, err)
	if !ok || c.ID != "old" || opt.Action != "saddle unstack t2" || c.Answer != 2 || c.By != DecidedByOwner {
		t.Fatalf("answered %+v %+v %v", c, opt, ok)
	}
	if line := c.Line(); strings.Contains(line, "\n") || !strings.Contains(line, "option 2") {
		t.Fatalf("collapsed line = %q", line)
	}
	if c, _, _, _ := a.AnswerDecision("1", now); c.ID != "young" {
		t.Fatalf("second reply answered %q, want young", c.ID)
	}
	if _, _, ok, _ := a.AnswerDecision("1", now); ok {
		t.Fatal("a reply with no open owner card was taken as an answer")
	}
	if _, _, ok, _ := a.AnswerDecision("hello", now); ok {
		t.Fatal("prose was taken as an answer")
	}
	if !hasEvent(t, a, EventDecisionAnswered, "old") {
		t.Fatal("answer not logged")
	}
}

// A reply past the card's option count is not an answer.
func TestAnswerDecisionOutOfRange(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	must(t, a.OpenDecision(testCard("x")))
	if _, _, ok, _ := a.AnswerDecision("3", time.Now()); ok {
		t.Fatal("option 3 of 2 accepted")
	}
	if open, _ := a.OpenDecisions(); len(open) != 1 {
		t.Fatalf("open = %d, want 1", len(open))
	}
}

// Re-opening a card that is still open neither duplicates it nor re-pings.
func TestOpenDecisionDedupes(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	must(t, a.OpenDecision(testCard("x")))
	must(t, a.OpenDecision(testCard("x")))
	if open, _ := a.OpenDecisions(); len(open) != 1 {
		t.Fatalf("open = %d, want 1", len(open))
	}
	ns, _ := a.Store.TakeNotices(OrchestratorID, false)
	if len(ns) != 1 || !strings.HasPrefix(ns[0].Text, "‼ ") {
		t.Fatalf("notices = %+v", ns)
	}
	bad := testCard("y")
	bad.Default = 0
	if a.OpenDecision(bad) == nil {
		t.Fatal("an invalid card opened")
	}
}

// #404 acceptance: an unanswered card times out, runs its default and logs it.
func TestExpireDecisionsRunsDefault(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	now := time.Now().UTC()
	c := testCard("x")
	c.Opened = now
	must(t, a.OpenDecision(c))
	var ran []string
	run := func(c DecisionCard, o DecisionOption) error { ran = append(ran, c.ID+":"+o.Action); return nil }

	if got, err := a.ExpireDecisions(now.Add(29*time.Minute), run); err != nil || len(got) != 0 {
		t.Fatalf("expired early: %+v %v", got, err)
	}
	got, err := a.ExpireDecisions(now.Add(31*time.Minute), run)
	must(t, err)
	if len(got) != 1 || got[0].By != DecidedByTimeout || got[0].Answer != 2 || len(ran) != 1 || ran[0] != "x:saddle unstack t2" {
		t.Fatalf("expired %+v, ran %v", got, ran)
	}
	if !hasEvent(t, a, EventDecisionDefault, "x") {
		t.Fatal("default not logged")
	}
	if open, _ := a.OpenDecisions(); len(open) != 0 {
		t.Fatalf("still open: %+v", open)
	}
	if got, _ := a.ExpireDecisions(now.Add(time.Hour), run); len(got) != 0 || len(ran) != 1 {
		t.Fatal("default ran twice")
	}
}

// A default that fails stays open, so the next cycle retries it.
func TestExpireDecisionsKeepsFailedDefaultOpen(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	c := testCard("x")
	c.Opened = time.Now().Add(-time.Hour)
	must(t, a.OpenDecision(c))
	_, err := a.ExpireDecisions(time.Now(), func(DecisionCard, DecisionOption) error { return errors.New("boom") })
	if err == nil {
		t.Fatal("failed default reported no error")
	}
	if open, _ := a.OpenDecisions(); len(open) != 1 {
		t.Fatal("failed default closed the card")
	}
}
