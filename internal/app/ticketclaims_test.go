package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// #908: tickets carried no Claims: line, so the orchestrator guessed globs.
// A spawn for an issue reads them: the Claims: line when there is one, else
// what the body's paths infer, minus globs another live task holds.
func TestIssueClaims(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	_, _ = originWithGh(t, a)
	write(t, a.Root, "text-game/src/queries.ts", "x\n")
	write(t, a.Root, "text-game/src/world.ts", "x\n")
	setIssue(t, 1, "declared", "Fix the queries.\n\nClaims: text-game/src/queries*\n")
	setIssue(t, 2, "inferred", "The bug is in `text-game/src/queries.ts` and `text-game/src/world.ts`.\n")
	setIssue(t, 3, "vague", "Make it faster.\n")

	if got, note := a.IssueClaims(1); len(got) != 1 || got[0] != "text-game/src/queries*" || !strings.Contains(note, "Claims: line") {
		t.Errorf("declared: %v, %q", got, note)
	}
	got, note := a.IssueClaims(2)
	if len(got) == 0 || !strings.Contains(note, "inferred") {
		t.Fatalf("inferred: %v, %q", got, note)
	}
	if got, note := a.IssueClaims(3); got != nil || !strings.Contains(note, "claims files as it goes") {
		t.Errorf("vague: %v, %q", got, note)
	}
	if got, note := a.IssueClaims(99); got != nil || !strings.Contains(note, "couldn't read #99") {
		t.Errorf("missing issue: %v, %q", got, note)
	}

	// Another live task holds one inferred glob: it is left out, not a refusal.
	must(t, a.Store.CreateTask(store.Task{ID: "t9", Title: "other", Role: store.RoleWorker, Status: store.Running}))
	_, err := a.Store.Claim("t9", func(map[string][]string) ([]string, error) { return []string{got[0]}, nil })
	must(t, err)
	again, note := a.IssueClaims(2)
	if len(again) != len(got)-1 || !strings.Contains(note, "held by t9") {
		t.Errorf("with %s held: %v, %q", got[0], again, note)
	}
}
