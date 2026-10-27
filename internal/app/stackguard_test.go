package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeGHLog installs a fake gh for a.Root that logs every call and answers
// the open-PR query with no PRs, and returns a reader for the log.
func fakeGHLog(t *testing.T, a *App) func() string {
	t.Helper()
	bin := filepath.Join(a.Root, ".git", FakeGHSubdir)
	must(t, os.MkdirAll(bin, 0o755))
	log := filepath.Join(bin, "gh.log")
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
case "$1 $2" in
"api graphql") echo '{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}' ;;
esac
`
	must(t, os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755))
	return func() string {
		b, _ := os.ReadFile(log)
		return string(b)
	}
}

// #751: gc deleted a merged task's branch while other PRs still targeted it,
// and GitHub closes a PR whose base branch is deleted by a push: about 25
// PRs in quark closed unmerged. gc now moves those PRs onto base first.
func TestGCRetargetsDependentsBeforeDeletingBranch(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	other := withOrigin(t, a)
	calls := fakeGHLog(t, a)
	merged := killedWithWork(t, a, "merged", "a.txt")
	if _, err := trainGit(a.Root, "push", "-q", "origin", merged.Branch); err != nil {
		t.Fatal(err)
	}
	git(t, a.Root, "branch", "-D", merged.Branch)
	pushCommit(t, other, "a.txt")
	git(t, a.Root, "fetch", "-q", "origin")
	dep := killedWithWork(t, a, "dependent", "d.txt")
	must(t, a.Store.SetField(dep.ID, "pr", "https://github.com/o/r/pull/9"))
	must(t, a.saveBase(dep.ID, merged.Branch))

	_, err := a.GC()
	must(t, err)
	if out := git(t, a.Root, "ls-remote", "origin", "refs/heads/"+merged.Branch); out != "" {
		t.Fatal("gc left the merged branch on origin")
	}
	if !strings.Contains(calls(), "pr edit https://github.com/o/r/pull/9 --base main") {
		t.Fatalf("gc deleted %s without retargeting the PR based on it; gh calls:\n%s", merged.Branch, calls())
	}
	if got := a.savedBases()[dep.ID]; got != "main" {
		t.Fatalf("saved base = %q, want main", got)
	}
}

// #751: a PR stacked on another task's branch merges into that branch, not
// main, and reviewers couldn't tell what a PR held or what to merge first.
// Each PR now gets a saddle/base commit status, red unless it targets main,
// and its stack comment says why it is stacked, what to merge first, and
// how much of its diff against main is other tasks' work.
func TestPRsGuardStackedLayers(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, calls := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "t2", "one more", map[string]string{"one.txt": "one\nmore\n"})
	_, err := a.PRs()
	must(t, err)
	t1, t2 = mustTask(t, a, t1.ID), mustTask(t, a, t2.ID)
	status := func(branch, state string) bool {
		sha := remoteRev(t, origin, branch)
		for _, c := range calls() {
			if strings.Contains(c, "statuses/"+sha) && strings.Contains(c, "state="+state) && strings.Contains(c, "context=saddle/base") {
				return true
			}
		}
		return false
	}
	if !status(t1.Branch, "success") {
		t.Errorf("t1 (on main) has no green saddle/base status:\n%s", strings.Join(calls(), "\n"))
	}
	if !status(t2.Branch, "failure") {
		t.Errorf("t2 (on t1's branch) has no red saddle/base status:\n%s", strings.Join(calls(), "\n"))
	}
	c2 := lastStackComment(t, t2.PR)
	for _, want := range []string{"Merge first: " + t1.PR, "Stacked because", "against `main`: 2 commits", "1 from the layers below"} {
		if !strings.Contains(c2, want) {
			t.Errorf("t2's stack comment lacks %q:\n%s", want, c2)
		}
	}
	c1 := lastStackComment(t, t1.PR)
	if !strings.Contains(c1, "against `main`: 1 commit,") || strings.Contains(c1, "layers below") {
		t.Errorf("t1's stack comment:\n%s", c1)
	}
}
