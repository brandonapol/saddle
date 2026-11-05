package sentinel

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/netx"
)

// timedOutCI is a ci-red forge whose gh calls run past their deadline.
type timedOutCI struct{ ciFake }

func (timedOutCI) PR(context.Context, string, forge.PRFields) (forge.PR, error) {
	return forge.PR{}, &netx.TimeoutError{Op: "gh pr view", After: time.Minute}
}

// #788: a gh call that timed out on a dead connection is a wait, like the
// rate limit: the ci-red watcher skips the cycle and logs no ci_red_error.
func TestCIRedRunOverNetTimeoutLogsNoError(t *testing.T) {
	a, _ := setup(t)
	newFakeGH(t)
	landTask(t, a, "t1", "f1")
	_, err := a.PRs()
	must(t, err)
	c := &CIRed{App: a, Forge: &timedOutCI{}, Interval: 5 * time.Millisecond, MaxInterval: 5 * time.Millisecond}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := c.Run(ctx); err != nil && !strings.Contains(err.Error(), "deadline") {
		t.Fatal(err)
	}
	noEvent(t, a, EventCIRedError)
}
