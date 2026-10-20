package doctor

import (
	"strings"
	"testing"
)

type cycleFake struct {
	*fakeEnv
	cycles []string
}

func (c cycleFake) HookCycles() ([]string, error) { return c.cycles, nil }

// #691: a <name>.pre-saddle wrapper that runs saddle's own ref guard is an
// error, not a warning.
func TestHooksCycleIsError(t *testing.T) {
	f := healthy(t)
	c := find(t, Run(cycleFake{f, []string{"reference-transaction", "commit-msg"}}), CheckHooks)
	if c.Status != Fail || !strings.Contains(c.Detail, "reference-transaction, commit-msg") || !strings.Contains(c.Fix, "saddle init") {
		t.Fatalf("got %+v", c)
	}
	if c := find(t, Run(cycleFake{f, nil}), CheckHooks); c.Status != OK {
		t.Fatalf("no cycle should pass: %+v", c)
	}
}
