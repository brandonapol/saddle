package forge

import (
	"context"
	"testing"
)

// #931: saddle no longer posts saddle/base, but open PRs still carry the
// red one it posted before ADR 0012 at their heads. Every read that the
// ci-red watcher (PR) and automerge (PR, open PRs) make leaves it out, so
// a PR red only on that stale status is neither red nor blocked.
func TestLegacyBaseStatusIsIgnoredOnRead(t *testing.T) {
	ctx := context.Background()
	view := `{"state":"OPEN","statusCheckRollup":[` +
		`{"__typename":"StatusContext","context":"saddle/base","state":"FAILURE"},` +
		`{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"}]}`
	g := &GitHub{Run: (&fakeGH{replies: []string{view}}).run}
	pr, err := g.PR(ctx, "1", PRState|PRChecks)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.Checks) != 1 || pr.Checks[0].Name != "test" {
		t.Fatalf("PR checks = %+v, want only test", pr.Checks)
	}

	page := `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[{"url":"u","state":"OPEN",` +
		`"statusCheckRollup":{"contexts":{"nodes":[{"__typename":"StatusContext","context":"saddle/base","state":"FAILURE"}]}}}]}}}}`
	prs, _, err := parseOpenPRs(page)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 || len(prs[0].Checks) != 0 {
		t.Fatalf("open PRs = %+v, want no checks", prs)
	}

	checks := `[{"name":"saddle/base","workflow":"","bucket":"fail","link":""},{"name":"test","workflow":"ci","bucket":"pass","link":"l"}]`
	g = &GitHub{Run: (&fakeGH{replies: []string{checks}}).run}
	cs, err := g.Checks(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Name != "test" {
		t.Fatalf("Checks = %+v, want only test", cs)
	}

	// Only that exact name: another saddle-looking status is still CI.
	if legacyStatus(CheckRun{Context: "saddle/other"}) || !legacyStatus(CheckRun{Name: "saddle/base"}) {
		t.Fatal("legacyStatus misclassifies")
	}
}
