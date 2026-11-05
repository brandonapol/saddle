package sentinel

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/forge"
)

// holdGH writes the governor's marker, as a gh call that hit the limit
// would, so every gh call from the repo is held (#764).
func holdGH(t *testing.T, a *app.App) {
	t.Helper()
	b, err := json.Marshal(forge.RateLimitHold{Until: time.Now().Add(time.Hour), Source: "gh pr view"})
	must(t, err)
	must(t, os.WriteFile(filepath.Join(a.Root, ".saddle", forge.RateLimitFile), b, 0o644))
}

func noEvent(t *testing.T, a *app.App, kind string) {
	t.Helper()
	es, err := a.Store.Events(1000)
	must(t, err)
	for _, e := range es {
		if e.Kind == kind {
			t.Fatalf("logged %s under the rate limit: %s", kind, e.Data)
		}
	}
}

// #764: a sentinel cycle under GitHub's rate limit logs no sentinel_error
// and no decision_default_failed. The case from quark: a ci-repair-cap card
// times out and its default labels the PR needs-human through gh. (The
// card's retry_after is TestExpireDecisionsRateLimitedDefaultWaits.)
func TestSentinelRunUnderRateLimitLogsNoError(t *testing.T) {
	a, _ := setup(t)
	newFakeGH(t)
	tk := landTask(t, a, "t1", "one")
	_, err := a.PRs()
	must(t, err)
	must(t, a.OpenDecision(app.DecisionCard{
		ID: "ci-repair-cap:t1", Kind: app.DecisionCIRepairCap, To: app.OrchestratorID, Task: tk.ID,
		Headline: "CI repair cap: t1 needs you.", What: "CI stayed red.", Blocking: "the stack.",
		Options: []app.DecisionOption{{Label: "Fix it", Action: "spawn a fix", Recommended: true}, {Label: "Drop it", Action: "saddle unstack t1", Run: app.RunDropRed}},
		Default: 2, Timeout: time.Minute, Opened: time.Now().Add(-time.Hour),
	}))
	holdGH(t, a)
	s := New(a)
	s.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = s.Run(ctx) // one cycle, then ctx is done
	noEvent(t, a, EventError)
	noEvent(t, a, app.EventDecisionDefaultFailed)
}

// limitedCI is a ci-red forge with GitHub's quota spent.
type limitedCI struct{ ciFake }

func (limitedCI) PR(context.Context, string, forge.PRFields) (forge.PR, error) {
	return forge.PR{}, &forge.RateLimitError{Until: time.Now().Add(time.Hour)}
}

// #764: the ci-red watcher skips a cycle under the rate limit and logs no
// ci_red_error.
func TestCIRedRunUnderRateLimitLogsNoError(t *testing.T) {
	a, _ := setup(t)
	newFakeGH(t)
	landTask(t, a, "t1", "f1")
	_, err := a.PRs()
	must(t, err)
	c := &CIRed{App: a, Forge: &limitedCI{}, Interval: 5 * time.Millisecond, MaxInterval: 5 * time.Millisecond}
	if _, err := c.Check(context.Background()); !errors.Is(err, forge.ErrRateLimited) {
		t.Fatalf("Check under the limit = %v; want ErrRateLimited", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.Run(ctx); err != nil && !strings.Contains(err.Error(), "deadline") {
		t.Fatal(err)
	}
	noEvent(t, a, EventCIRedError)
}
