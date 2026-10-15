package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/gitx"
)

// publishSetup is a repo with a PR template on main, an origin, a fake gh and
// saddle's ref guard installed, so publish's push meets the pre-push hook.
func publishSetup(t *testing.T) (*App, string, func() []string) {
	t.Helper()
	a := trainSetup(t)
	write(t, a.Root, ".github/pull_request_template.md", "## Summary\n<!-- what and why -->\n\n## Testing\n- [ ] make check\n")
	commitAll(t, a.Root, "template")
	must(t, a.Init())
	origin, ghLog := originWithGh(t, a)
	return a, origin, ghLog
}

// prCreate returns the gh pr create call for head in the log, joined across
// the lines a multi-line body spreads it over.
func prCreate(log []string, head string) string {
	all := strings.Join(log, "\n")
	for _, call := range strings.Split(all, "pr create ")[1:] {
		if strings.Contains(call, "--head "+head+" ") {
			return call
		}
	}
	return ""
}

func TestPublishLandedTasksAsIndependentPRs(t *testing.T) {
	t.Parallel()
	a, origin, ghLog := publishSetup(t)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	tk, err := a.Spawn(SpawnReq{ID: "t2", Title: "two"})
	must(t, err)
	write(t, tk.Worktree, "two.txt", "two\n")
	commitAll(t, tk.Worktree, "add two\n\nCloses #42")
	write(t, tk.Worktree, "three.txt", "three\n")
	commitAll(t, tk.Worktree, "add three")
	must(t, a.Done(tk.ID, "two and three"))
	if _, err := a.Land(); err != nil {
		t.Fatal(err)
	}
	// The stack is broken: prs refuses, publish is the way out.
	must(t, a.SetFlag(StackFlag{Task: t1.ID, Cause: "GitHub refuses to change the base of a stacked PR"}))
	if _, err := a.PRs(); err == nil {
		t.Fatal("prs went ahead on a flagged stack")
	}
	main := git(t, a.Root, "rev-parse", "main")

	for _, c := range []struct {
		id, branch string
		own        string // the task's own commits on integration
		closes     string
	}{
		{"t2", "fix/42-two", a.Cfg.Integration + "~2.." + a.Cfg.Integration, "Closes #42"},
		{"t1", "", a.Cfg.Integration + "~3.." + a.Cfg.Integration + "~2", ""},
	} {
		res, err := a.Publish(PublishReq{Target: c.id, BranchName: c.branch})
		if err != nil {
			t.Fatalf("publish %s: %v", c.id, err)
		}
		if c.branch == "" && res.Branch != "saddle/one" {
			t.Fatalf("publish %s: default branch = %q, want saddle/one", c.id, res.Branch)
		} else if c.branch != "" && res.Branch != c.branch {
			t.Fatalf("publish %s: branch = %q", c.id, res.Branch)
		}
		head := remoteRev(t, origin, res.Branch)
		if head == "" {
			t.Fatalf("publish %s: %s not pushed", c.id, res.Branch)
		}
		got, err := gitx.PatchIDs(a.Root, main+".."+head)
		must(t, err)
		want, err := gitx.PatchIDs(a.Root, c.own)
		must(t, err)
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Fatalf("publish %s: %s holds %v, want only its own %v", c.id, res.Branch, got, want)
		}
		call := prCreate(ghLog(), res.Branch)
		if call == "" || !strings.Contains(call, "--base main") {
			t.Fatalf("publish %s: no pr create against main for %s:\n%v", c.id, res.Branch, ghLog())
		}
		if !strings.Contains(call, "## Testing") || !strings.Contains(call, "- [ ] make check") {
			t.Fatalf("publish %s: body doesn't follow the PR template: %s", c.id, call)
		}
		if strings.Contains(call, "<!-- what and why -->") {
			t.Fatalf("publish %s: template placeholder left unfilled: %s", c.id, call)
		}
		if c.closes != "" && strings.Count(call, c.closes) != 1 {
			t.Fatalf("publish %s: body should close the issue once: %s", c.id, call)
		}
		got2, err := a.Store.Task(c.id)
		must(t, err)
		if got2.PR != res.URL || res.URL == "" {
			t.Fatalf("publish %s: task PR = %q, result %q", c.id, got2.PR, res.URL)
		}
	}
	// Neither task's own branch moved on origin.
	for _, id := range []string{"t1", "t2"} {
		tk, err := a.Store.Task(id)
		must(t, err)
		if r := remoteRev(t, origin, tk.Branch); r != "" {
			t.Fatalf("publish pushed %s's stack branch %s", id, tk.Branch)
		}
	}
}

func TestPublishRefusesWorkFromBelow(t *testing.T) {
	t.Parallel()
	a, origin, ghLog := publishSetup(t)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"one.txt": "one\ntwo\n"})

	_, err := a.Publish(PublishReq{Target: "t2"})
	if err == nil || !strings.Contains(err.Error(), "t1") {
		t.Fatalf("publish of work that needs t1: err = %v", err)
	}
	if r := remoteRev(t, origin, "saddle/two"); r != "" {
		t.Fatal("pushed anyway")
	}
	if l := ghLog(); len(l) > 0 {
		t.Fatalf("gh called: %v", l)
	}
}

func TestPublishBranchRefusesOtherTasksCommits(t *testing.T) {
	t.Parallel()
	a, origin, _ := publishSetup(t)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	git(t, a.Root, "branch", "fix/mine", a.Cfg.Integration)
	git(t, a.Root, "worktree", "add", "-q", a.stateDir("fixwt"), "fix/mine")
	write(t, a.stateDir("fixwt"), "mine.txt", "mine\n")
	commitAll(t, a.stateDir("fixwt"), "mine")

	_, err := a.Publish(PublishReq{Target: "fix/mine"})
	if err == nil || !strings.Contains(err.Error(), "t1") {
		t.Fatalf("publish of a branch carrying t1's commit: err = %v", err)
	}
	if r := remoteRev(t, origin, "fix/mine"); r != "" {
		t.Fatal("pushed anyway")
	}
}

func TestPublishExistingPRPrintsURL(t *testing.T) {
	t.Parallel()
	a, origin, ghLog := publishSetup(t)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	must(t, a.Store.SetField(t1.ID, "pr", "https://github.com/o/r/pull/7"))

	res, err := a.Publish(PublishReq{Target: "t1"})
	must(t, err)
	if !res.Existing || res.URL != "https://github.com/o/r/pull/7" {
		t.Fatalf("publish with a PR open: %+v", res)
	}
	if r := remoteRev(t, origin, "saddle/one"); r != "" || len(ghLog()) > 0 {
		t.Fatal("publish pushed or called gh when the PR exists")
	}
}

func TestPublishRefusesAnotherTasksBranch(t *testing.T) {
	t.Parallel()
	a, _, _ := publishSetup(t)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	for _, name := range []string{t1.Branch, a.Cfg.Integration, "main"} {
		if _, err := a.Publish(PublishReq{Target: "t2", BranchName: name}); err == nil {
			t.Fatalf("publish onto %s was allowed", name)
		}
	}
}

func TestPRsLeavesPublishedTaskAlone(t *testing.T) {
	t.Parallel()
	a, origin, ghLog := publishSetup(t)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	res, err := a.Publish(PublishReq{Target: "t1"})
	must(t, err)
	before := len(ghLog())
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	for _, l := range ghLog()[before:] {
		if strings.HasPrefix(l, "pr edit "+res.URL) || strings.HasPrefix(l, "pr create") {
			t.Fatalf("prs touched the independently published PR: %s", l)
		}
	}
	if r := remoteRev(t, origin, t1.Branch); r != "" {
		t.Fatalf("prs pushed %s after it was published on its own", t1.Branch)
	}
}

// #336: publish --base of a branch in the PR stack opens a PR the next prs
// drops, because prs puts that branch back on the commit the train landed
// (see TestPRMergedIntoStackedBaseGetsFreshPR). Refuse it and point at prs
// and restack. Publishing against the configured base still works.
func TestPublishRefusesStackBranchBase(t *testing.T) {
	t.Parallel()
	a, origin, ghLog := publishSetup(t)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})

	_, err := a.Publish(PublishReq{Target: "t2", Base: t1.Branch})
	if err == nil || !strings.Contains(err.Error(), t1.Branch) || !strings.Contains(err.Error(), "prs") || !strings.Contains(err.Error(), "restack") {
		t.Fatalf("publish --base %s: %v", t1.Branch, err)
	}
	if remoteRev(t, origin, "saddle/two") != "" {
		t.Fatal("pushed a PR branch anyway")
	}
	if len(ghLog()) != 0 {
		t.Fatalf("gh called: %v", ghLog())
	}

	res, err := a.Publish(PublishReq{Target: "t1", Base: a.Cfg.Base})
	if err != nil {
		t.Fatalf("publish against %s: %v", a.Cfg.Base, err)
	}
	if remoteRev(t, origin, res.Branch) == "" {
		t.Fatal("publish against the configured base pushed nothing")
	}
}

// #336: publish re-reads [train] lint.cmd before the pre-publish gate, so a
// long-lived process runs the command edited after it started.
func TestPublishReloadsLintCmd(t *testing.T) {
	t.Parallel()
	a, origin, _ := publishSetup(t)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})

	write(t, a.Root, ".saddle/config.toml", "[test]\ncmd = \"none\"\n[train]\nlint.cmd = 'echo publish-old-gate; exit 1'\n")
	_, err := a.Publish(PublishReq{Target: "t1", BranchName: "fix/one"})
	if err == nil || !strings.Contains(err.Error(), "echo publish-old-gate; exit 1") {
		t.Fatalf("publish with lint.cmd edited after startup: %v", err)
	}
	if remoteRev(t, origin, "fix/one") != "" {
		t.Fatal("publish pushed past the reloaded red gate")
	}

	write(t, a.Root, ".saddle/config.toml", "[test]\ncmd = \"none\"\n[train]\nlint.cmd = \"true\"\n")
	res, err := a.Publish(PublishReq{Target: "t1", BranchName: "fix/one"})
	if err != nil {
		t.Fatalf("publish after lint.cmd changed to a green gate: %v", err)
	}
	if remoteRev(t, origin, res.Branch) == "" {
		t.Fatal("publish did not push once the reloaded gate was green")
	}
}
