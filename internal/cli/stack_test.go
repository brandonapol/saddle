package cli

import (
	"strings"
	"testing"
)

// stack collapse refuses a task that isn't in the PR stack.
func TestStackCollapseRefusesUnlanded(t *testing.T) {
	a := automergeRepo(t) // t1 spawned, not landed
	t.Chdir(a.Root)
	if out, err := runCmd(t, Root(), "stack", "collapse", "t1"); err == nil || !strings.Contains(err.Error(), "isn't in the PR stack") {
		t.Fatalf("collapse of an unlanded task: %v\n%s", err, out)
	}
}
