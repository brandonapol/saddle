package app

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// conventionalTitleRe is #951's acceptance check on a PR title.
var conventionalTitleRe = regexp.MustCompile(`^(feat|fix|chore|refactor|docs|test|perf)(\(.+\))?: `)

// doneCreateCall is the one "pr create" call done made for branch, from the
// fake gh's log.
func doneCreateCall(t *testing.T, a *App, branch string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(a.Root, ".git", FakeGHSubdir, "gh.log"))
	call := onlyCall(prCreate([]string{string(b)}, branch))
	if call == "" {
		t.Fatalf("no pr create for %s:\n%s", branch, b)
	}
	return strings.TrimRight(call, "\n")
}

// titleAndBody splits a logged "pr create" call into its title and body.
func titleAndBody(t *testing.T, call string) (string, string) {
	t.Helper()
	_, rest, ok := strings.Cut(call, "--title ")
	if !ok {
		t.Fatalf("no title in %q", call)
	}
	title, body, ok := strings.Cut(rest, " --body ")
	if !ok {
		t.Fatalf("no body in %q", call)
	}
	return title, body
}

// #951: done published autobutler-org/quark#4241 with the task's title, a
// one-paragraph body ending in the Claude Code footer (which quark's
// check/attribution job rejects), and "Saddle-Task: t794" as visible text.
// Done now lays the summary into the repo's PR template, takes the title
// from the worker's conventional commit, drops the footer, and hides the
// task trailer in an HTML comment.
func TestDonePRFollowsRepoRules(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	write(t, a.Root, ".github/pull_request_template.md", quarkTemplate)
	commitAll(t, a.Root, "chore: add the PR template")
	doneGH(t, a)
	tk, err := a.Spawn(SpawnReq{ID: "t1", Title: "Login crashes on an empty token", Issue: 4241})
	must(t, err)
	commitFiles(t, tk.Worktree, "fix(auth): stop the login crash on an empty token", map[string]string{"login.go": "package x\n", "login_test.go": "package x\n"})
	must(t, a.Done(tk.ID, "Stops the login handler crashing on an empty token.\n\n🤖 Generated with [Claude Code](https://claude.com/claude-code)"))

	title, body := titleAndBody(t, doneCreateCall(t, a, tk.Branch))
	if title != "fix(auth): stop the login crash on an empty token" || !conventionalTitleRe.MatchString(title) {
		t.Fatalf("title = %q, want the worker's conventional commit subject", title)
	}
	if strings.Contains(body, "Generated with") || strings.Contains(body, "🤖") {
		t.Fatalf("body carries the attribution footer:\n%s", body)
	}
	if !strings.HasSuffix(body, "<!--\nSaddle-Task: t1\n-->") {
		t.Fatalf("body doesn't end with the task trailer in an HTML comment:\n%s", body)
	}
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "Saddle-Task:") && !strings.Contains(body, "<!--\n"+l+"\n-->") {
			t.Fatalf("Saddle-Task shown as visible text:\n%s", body)
		}
	}
	checkQuarkBody(t, body, "#4241")
	for _, want := range []string{"Stops the login handler crashing", "- [x] Bug fix\n", "- [x] Backend (Go)\n", "- [x] Includes new automated tests\n"} {
		if !strings.Contains(body+"\n", want) {
			t.Fatalf("body lacks %q:\n%s", want, body)
		}
	}
}

// A worker whose commits aren't conventional, in a repo whose history is,
// still gets a conventional title: the change's kind and the task's title.
func TestDonePRTitleFollowsAConventionalHistory(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	for _, s := range []string{"feat: one", "fix: two", "chore: three"} {
		commitFiles(t, a.Root, s, map[string]string{strings.Fields(s)[1] + ".txt": s + "\n"})
	}
	doneGH(t, a)
	tk, err := a.Spawn(SpawnReq{ID: "t1", Title: "Add the login page"})
	must(t, err)
	commitFiles(t, tk.Worktree, "login page", map[string]string{"login.go": "package x\n"})
	must(t, a.Done(tk.ID, "Adds it."))
	title, _ := titleAndBody(t, doneCreateCall(t, a, tk.Branch))
	if title != "feat: add the login page" {
		t.Fatalf("title = %q, want %q", title, "feat: add the login page")
	}
}

// A repo that doesn't write conventional commits keeps the task's title.
func TestDonePRTitleKeepsTaskTitleWithoutConventions(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	doneGH(t, a)
	tk, err := a.Spawn(SpawnReq{ID: "t1", Title: "Add the login page"})
	must(t, err)
	commitFiles(t, tk.Worktree, "login page", map[string]string{"login.go": "package x\n"})
	must(t, a.Done(tk.ID, "Adds it."))
	title, body := titleAndBody(t, doneCreateCall(t, a, tk.Branch))
	if title != "Add the login page" {
		t.Fatalf("title = %q, want the task's title", title)
	}
	if body != "Adds it.\n\n<!--\nSaddle-Task: t1\n-->" {
		t.Fatalf("body = %q", body)
	}
}

// #951 moved the PR body's Saddle-Task line into an HTML comment. GitHub's
// squash copies the comment into the merge commit as is, and the line still
// says the task merged.
func TestTaskTrailerInAnHTMLCommentStillCounts(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	squashIntoMain(t, origin, "fix: one (#1)\n\n"+withTaskTrailer("one summary", "t1")+"\nCo-authored-by: Reviewer <r@example.com>\n",
		map[string]string{"one.txt": "one\n"})
	git(t, a.Root, "fetch", "-q", "origin")
	git(t, a.Root, "checkout", "-q", "-b", "earlier", "origin/main~1")
	write(t, a.Root, "earlier.txt", "earlier\n")
	git(t, a.Root, "add", "-A")
	git(t, a.Root, "-c", "core.hooksPath=/dev/null", "commit", "-q", "-m", "earlier", "--date=2001-01-01T00:00:00")
	if !a.taskTrailerOn("t1", "origin/main", "earlier") {
		t.Fatal("t1's commented trailer on base wasn't found")
	}
}
