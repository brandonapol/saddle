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

// ADR 0012 rule 7 (#931): a check on a saddle PR is always the repo's own
// CI. Saddle used to post a red status on every stacked PR and a pasted
// stack comment that went stale; publishing and restacking a stack now post
// neither, and leave the author's PR body alone. The forge interface has no
// way to post a status, so this also guards a raw `gh api .../statuses`.
func TestPRsAndRestackPostNoStatusOrComment(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	_, calls := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "t2", "one more", map[string]string{"one.txt": "one\nmore\n"})
	must(t, a.Store.SetField(t2.ID, "summary", "Author description\n- [ ] checklist"))
	_, err := a.PRs()
	must(t, err)
	if t2 = mustTask(t, a, t2.ID); t2.PR == "" {
		t.Fatal("t2 has no PR")
	}
	_, err = a.Restack()
	must(t, err)
	_, err = a.PRs()
	must(t, err)
	for _, c := range calls() {
		switch {
		case strings.Contains(c, "statuses/"):
			t.Errorf("posted a commit status: %s", c)
		case strings.HasPrefix(c, "pr comment "), strings.Contains(c, "updateIssueComment"):
			t.Errorf("posted or edited a PR comment: %s", c)
		case strings.HasPrefix(c, "pr edit ") && strings.Contains(c, "--body"):
			t.Errorf("overwrote the author's PR body: %s", c)
		}
	}
}
