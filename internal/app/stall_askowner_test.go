package app

import (
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

func askOwnerSetup(t *testing.T) *App {
	t.Helper()
	a, _ := setup(t)
	a.Cfg.Serial = []string{"go.mod"}
	must(t, a.Store.CreateTask(store.Task{ID: "t5", Title: "cache layer", Role: store.RoleWorker, Status: store.Running}))
	must(t, a.Store.CreateTask(store.Task{ID: "t6", Title: "billing", Role: store.RoleWorker, Status: store.Running}))
	must(t, a.Store.CreateTask(store.Task{ID: "t7", Title: "other", Role: store.RoleWorker, Status: store.Running}))
	if _, err := a.Store.Claim("t6", func(map[string][]string) ([]string, error) { return []string{"billing/**"}, nil }); err != nil {
		t.Fatal(err)
	}
	return a
}

// #794: the owner answers an ask_owner card with saddle message; the card
// closes as answered with the message, and its "go ahead" default never
// runs over the real answer.
func TestAskOwnerAnswerClosesCard(t *testing.T) {
	t.Parallel()
	a := askOwnerSetup(t)
	now := time.Now().UTC()
	_, err := a.AskOwner("t5", "go.mod", "May I bump golang.org/x/tools?")
	must(t, err)
	_, _ = a.Store.TakeNotices("t5", false)
	must(t, a.Message("user", "t5", "No: leave go.mod alone."))
	cs, err := a.Decisions()
	must(t, err)
	if len(cs) != 1 || cs[0].Open() || cs[0].By != DecidedByOwner || cs[0].Reply != "No: leave go.mod alone." {
		t.Fatalf("card after the answer = %+v, want closed as answered with the message", cs)
	}
	if _, err := a.StallTick(now.Add(31 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	ns := noticesFor(t, a, "t5")
	if !slices.ContainsFunc(ns, func(n store.Notice) bool { return strings.Contains(n.Text, "leave go.mod alone") }) {
		t.Fatalf("t5 didn't get the answer: %+v", ns)
	}
	if slices.ContainsFunc(ns, func(n store.Notice) bool { return strings.Contains(n.Text, "No answer") }) {
		t.Fatalf("t5 was told nobody answered after the owner did: %+v", ns)
	}
}

// #794: a task that owns the path answers by messaging the asker; a
// message from anyone else leaves the card open.
func TestAskOwnerAnswerFromOwningTask(t *testing.T) {
	t.Parallel()
	a := askOwnerSetup(t)
	owner, err := a.AskOwner("t5", "billing/meter.go", "May I add a field to Meter?")
	must(t, err)
	if owner != "t6" {
		t.Fatalf("owner = %q", owner)
	}
	must(t, a.Message("t7", "t5", "unrelated"))
	if open := openCardsOf(t, a, DecisionAskOwner); len(open) != 1 {
		t.Fatalf("a bystander's message closed the card: %+v", open)
	}
	must(t, a.Message("t6", "t5", "yes, add it"))
	if open := openCardsOf(t, a, DecisionAskOwner); len(open) != 0 {
		t.Fatalf("the owner's answer left the card open: %+v", open)
	}
}

// #794: two processes expiring the same due card run its default once.
func TestExpireDecisionsRunsDefaultOnce(t *testing.T) {
	t.Parallel()
	a := askOwnerSetup(t)
	now := time.Now().UTC()
	c := askOwnerCard("t5", "t5", OrchestratorID, "go.mod", "go.mod", "may I?")
	c.Opened = now
	must(t, a.OpenDecision(c))
	var runs atomic.Int32
	run := func(DecisionCard, DecisionOption) error {
		runs.Add(1)
		time.Sleep(50 * time.Millisecond) // the other process reads the card meanwhile
		return nil
	}
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if _, err := a.ExpireDecisions(now.Add(31*time.Minute), run); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := runs.Load(); n != 1 {
		t.Fatalf("default ran %d times, want once", n)
	}
}
