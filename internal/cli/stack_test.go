package cli

import (
	"encoding/json"
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

// #166: saddle stack --json is valid JSON the mod's snapshot can parse,
// with the stacks and auto-merge state, even with GitHub out of reach.
func TestStackJSON(t *testing.T) {
	a := automergeRepo(t)
	t.Chdir(a.Root)
	out, err := runCmd(t, Root(), "stack", "--json")
	if err != nil {
		t.Fatalf("stack --json: %v\n%s", err, out)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stack --json is not JSON: %v\n%s", err, out)
	}
	if _, ok := got["enabled"]; !ok {
		t.Fatalf("stack --json lacks enabled:\n%s", out)
	}
}
