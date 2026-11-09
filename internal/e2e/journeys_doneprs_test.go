//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/mcpserver"
)

// TestJourneyDonePublishesOwnPR (ADR 0012, #932, #933): with done's default
// path, two agents on disjoint work each get one PR against main holding only
// their own commit, opened by done itself: no train, no land, no prs.
func TestJourneyDonePublishesOwnPR(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{Publish: true})
	open := func(v mcpserver.TaskView) bool { return v.Status == "pr_open" && v.PR != "" }
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, finished("alpha", "alpha\n")...)
	w.Spawn("t2", "Beta work", []string{"beta/**"}, finished("beta", "beta\n")...)
	urls := map[string]string{}
	for _, id := range []string{"t1", "t2"} {
		urls[id] = w.WaitTask(id, "its PR open", open).PR
	}
	if urls["t1"] == urls["t2"] {
		t.Fatalf("both tasks share PR %s", urls["t1"])
	}
	s, err := w.GH.Load()
	if err != nil {
		t.Fatal(err)
	}
	for id, dir := range map[string]string{"t1": "alpha", "t2": "beta"} {
		p := prNumber(t, s, urls[id])
		if p.Base != "main" {
			t.Errorf("%s's PR targets %s, want main", id, p.Base)
		}
		files := w.Git(w.Origin, "diff", "--name-only", "main..."+p.Head)
		if strings.TrimSpace(files) != dir+"/work.txt" {
			t.Errorf("%s's PR changes %q, want only %s/work.txt", id, files, dir)
		}
		if v := w.Task(id); v.Train != "" {
			t.Errorf("%s is in the train: %q", id, v.Train)
		}
	}
	if n := len(s.PRs); n != 2 {
		t.Fatalf("fake GitHub has %d PRs, want 2", n)
	}
}
