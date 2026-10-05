//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// TestJourneyPublishWithBrokenStack (#220): t1 and t2 land as one linear
// stack, then the stack breaks, so prs refuses. saddle publish puts each
// task's own commits up as an independent PR against main, with the repo's PR
// template filled in; the saddle process pushes, past the ref guard, and no
// task's stack branch moves.
func TestJourneyPublishWithBrokenStack(t *testing.T) {
	w := world(t, Options{Tables: "[train]\noutput = \"single\"\n"})
	w.WriteFile(".github/pull_request_template.md", "## Summary\n<!-- what changed and why -->\n\n## Test plan\n- [ ] make check\n")
	w.Git(w.Repo, "add", ".github")
	w.Git(w.Repo, "commit", "-q", "-m", "PR template")
	w.Git(w.Repo, "push", "-q", "origin", "main")

	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, finished("alpha", "alpha\n")...)
	w.Spawn("t2", "Beta work", []string{"beta/**"}, finished("beta", "beta\n")...)
	for _, id := range []string{"t1", "t2"} {
		w.WaitTask(id, "queued in the train", func(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" })
	}
	w.MustSaddle("land")

	must(t, w.App().SetFlag(app.StackFlag{Task: "t1", Cause: "Cannot change the base branch because the pull request is part of a stack"}))
	if r := w.Saddle("prs"); r.Code == 0 {
		t.Fatalf("prs went ahead on a broken stack: %s", r)
	}

	for _, c := range []struct{ id, branch, own, other string }{
		{"t1", "", "alpha/work.txt", "beta/work.txt"},
		{"t2", "fix/beta-work", "beta/work.txt", "alpha/work.txt"},
	} {
		args := []string{"publish", c.id}
		if c.branch != "" {
			args = append(args, "--branch-name", c.branch)
		}
		r := w.MustSaddle(args...)
		url := w.Task(c.id).PR
		if url == "" || !strings.Contains(r.Stdout, url) {
			t.Fatalf("publish %s: no PR recorded: %s", c.id, r)
		}
		pr := prNumber(t, w.GHState(), url)
		if pr.Base != "main" {
			t.Fatalf("%s's PR targets %s, want main", c.id, pr.Base)
		}
		if c.branch != "" && pr.Head != c.branch {
			t.Fatalf("%s's PR head = %s, want %s", c.id, pr.Head, c.branch)
		}
		if !strings.Contains(pr.Body, "## Test plan") || strings.Contains(pr.Body, "<!-- what changed") {
			t.Fatalf("%s's PR body doesn't fill the template:\n%s", c.id, pr.Body)
		}
		if w.OriginFile(pr.Head, c.own) == "" || w.OriginFile(pr.Head, c.other) != "" {
			t.Fatalf("%s's PR doesn't hold exactly its own work", c.id)
		}
		if again := w.MustSaddle("publish", c.id); !strings.Contains(again.Stdout, "already has a PR: "+url) {
			t.Fatalf("publish %s again: %s", c.id, again)
		}
	}
	if n := len(w.GHState().PRs); n != 2 {
		t.Fatalf("fake GitHub has %d PRs, want 2", n)
	}
	for _, id := range []string{"t1", "t2"} {
		tk, err := w.App().Store.Task(id)
		must(t, err)
		if w.Git(w.Origin, "branch", "--list", tk.Branch) != "" {
			t.Fatalf("publish pushed %s's stack branch", id)
		}
	}
}
