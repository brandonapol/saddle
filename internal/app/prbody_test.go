package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// #338: the owner strips the attribution footer from t2's PR, then merges
// it into t1's branch instead of main. Restack detaches the PR and prs opens
// a fresh one; that PR carries the stripped body as it stood, not a body
// rebuilt from t2's done summary with the footer back on.
func TestFreshPRAfterRelandKeepsTheEditedBody(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	must(t, a.Store.SetField("t2", "summary", "Adds two.\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)"))
	_, err := a.PRs()
	must(t, err)
	t1, _ := a.Store.Task("t1")
	t2, _ := a.Store.Task("t2")

	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	git(t, other, "checkout", "-q", t1.Branch)
	git(t, other, "merge", "-q", "--no-ff", "-m", "Merge pull request #2", "origin/"+t2.Branch)
	git(t, other, "push", "-q", "origin", t1.Branch)
	const edited = "Adds two, edited by the owner."
	setPRBody(t, t2.PR, "MERGED", t1.Branch, edited)

	before := len(ghLog())
	if _, err := a.PRs(); err != nil {
		t.Fatalf("PRs: %v", err)
	}
	var created []string
	for _, c := range ghLog()[before:] {
		if strings.HasPrefix(c, "pr create") && strings.Contains(c, "--head "+t2.Branch) {
			created = append(created, c)
		}
	}
	if len(created) != 1 {
		t.Fatalf("pr creates for t2 = %v, want one", created)
	}
	if !strings.HasSuffix(created[0], "--body "+edited) {
		t.Fatalf("fresh PR = %q, want the edited body %q untouched", created[0], edited)
	}
	if strings.Contains(created[0], "Generated with") {
		t.Fatalf("fresh PR put the attribution footer back: %q", created[0])
	}
}

// A task that never lost a PR still gets its body from its summary.
func TestStackPRBodyWithoutAnEarlierPRIsTheSummary(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	_, _ = originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	all, err := a.landedAll()
	must(t, err)
	for _, l := range all {
		if l.ID == "t1" {
			if got := a.stackPRBody(l); got != l.Summary {
				t.Fatalf("stackPRBody = %q, want the summary %q", got, l.Summary)
			}
			return
		}
	}
	t.Fatal("t1 didn't land")
}

// #557: a repo with no PR template still gets "Closes #N" for a task that
// implements an issue, once, and keeps any attribution footer last.
func TestStackPRBodyWithoutATemplateClosesTheIssue(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	const footer = "🤖 Generated with [Claude Code](https://claude.com/claude-code)"
	for _, c := range []struct {
		name, summary string
		issue         int
		want          string
	}{
		{"adds the line", "Adds one.", 7, "Adds one.\n\nCloses #7"},
		{"already closes", "Adds one.\n\nCloses #7", 7, "Adds one.\n\nCloses #7"},
		{"no issue", "Adds one.", 0, "Adds one."},
		{"footer stays last", "Adds one.\n\n" + footer, 7, "Adds one.\n\nCloses #7\n\n" + footer + "\n"},
	} {
		l := landedTask{Task: store.Task{ID: "t9", Summary: c.summary, Issue: c.issue}}
		if got := a.stackPRBody(l); got != c.want {
			t.Errorf("%s: stackPRBody = %q, want %q", c.name, got, c.want)
		}
	}
}

// setPRBody is setPR for a PR whose body is body.
func setPRBody(t *testing.T, url, state, baseRef, body string) {
	t.Helper()
	b := fmt.Sprintf(`{"state":%q,"mergeable":"MERGEABLE","baseRefName":%q,"body":%q}`, state, baseRef, body)
	must(t, os.WriteFile(filepath.Join(fakeGHDir(t), "view-"+filepath.Base(url)), []byte(b), 0o644))
}
