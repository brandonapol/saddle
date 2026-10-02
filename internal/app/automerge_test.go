package app

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/store"
)

// squashGH is a GitHub where every open PR is green and CLEAN, and merging
// one squashes its branch into origin's main from a clone, as GitHub does.
type squashGH struct {
	t      *testing.T
	a      *App
	clone  string
	merged map[string]bool
	order  []string
}

func newSquashGH(t *testing.T, a *App, origin string) *squashGH {
	clone := filepath.Join(t.TempDir(), "gh")
	git(t, a.Root, "clone", "-q", origin, clone)
	return &squashGH{t: t, a: a, clone: clone, merged: map[string]bool{}}
}

func (g *squashGH) task(url string) store.Task {
	ts, err := g.a.Store.Tasks()
	must(g.t, err)
	for _, tk := range ts {
		if tk.PR == url {
			return tk
		}
	}
	g.t.Fatalf("no task for %s", url)
	return store.Task{}
}

func (g *squashGH) PR(url string) (automerge.PR, error) {
	if g.merged[url] {
		return automerge.PR{URL: url, State: "MERGED"}, nil
	}
	tk := g.task(url)
	base := g.a.Cfg.Base
	// The PR's base is whatever the last pr create/edit said; read it from the fake gh log.
	for _, c := range ghCalls(g.t) {
		if strings.HasPrefix(c, "pr edit "+url+" --base ") {
			base = strings.TrimPrefix(c, "pr edit "+url+" --base ")
		}
	}
	head := git(g.t, g.a.Root, "rev-parse", "refs/remotes/origin/"+tk.Branch)
	return automerge.PR{URL: url, State: "OPEN", Mergeable: "MERGEABLE", MergeState: "CLEAN",
		Base: base, Head: tk.Branch, HeadSHA: head, Checks: automerge.ChecksPass}, nil
}

func (g *squashGH) MergeMethod() (string, error) { return "squash", nil }

func (g *squashGH) Merge(url, method, head string) error {
	tk := g.task(url)
	git(g.t, g.clone, "fetch", "-q", "origin")
	git(g.t, g.clone, "reset", "-q", "--hard", "origin/main")
	git(g.t, g.clone, "merge", "-q", "--squash", head)
	git(g.t, g.clone, "commit", "-qm", tk.Title+" (squash)")
	git(g.t, g.clone, "push", "-q", "origin", "main")
	g.merged[url] = true
	setPR(g.t, url, "MERGED", "main") // what restack's own gh lookup sees
	g.order = append(g.order, tk.ID)
	return nil
}

func ghCalls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(os.Getenv(fakeGHEnv), "gh.log"))
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func noticesFor(t *testing.T, a *App, task string) []store.Notice {
	t.Helper()
	ns, err := a.Store.TakeNotices(task, false)
	must(t, err)
	return ns
}

// #152: wired to the real stack, the watcher merges the bottom PR, restack
// retargets the next one to main, and the next tick merges that one.
func TestAutomergeMergesStackBottomUp(t *testing.T) {
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	origin, _ := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	_, err := a.PRs()
	must(t, err)

	gh := newSquashGH(t, a, origin)
	w := a.NewAutomerge(gh)
	st, err := w.Check()
	must(t, err)
	if st.Enabled || len(gh.order) != 0 {
		t.Fatalf("merged while off by default: %+v", gh.order)
	}
	must(t, w.SetEnabled(true))
	for range 3 {
		st, err = w.Check()
		must(t, err)
		if st.Stopped != "" {
			t.Fatalf("stopped: %s", st.Stopped)
		}
	}
	if !slices.Equal(gh.order, []string{"t1", "t2"}) {
		t.Fatalf("merged %v, want t1 then t2", gh.order)
	}
	evs, err := a.Store.Events(-1)
	must(t, err)
	var merged []string
	for _, e := range evs {
		if e.Kind == automerge.EventMerged {
			merged = append(merged, e.Task)
		}
	}
	if !slices.Equal(merged, []string{"t1", "t2"}) {
		t.Fatalf("merge events %v", merged)
	}
	if es, _ := a.automergeEntries(); len(es) != 0 {
		t.Fatalf("stack not empty after auto-merge: %+v", es)
	}
}

// Holding by PR number holds the task's stack; the state accessor (for the
// TUI) shows it without asking GitHub; a held stack never merges.
func TestAutomergeHoldByPR(t *testing.T) {
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	_, err := a.PRs()
	must(t, err)
	t1, _ := a.Store.Task("t1")
	n := t1.PR[strings.LastIndex(t1.PR, "/")+1:]

	tk, err := a.AutomergeHold("#" + n)
	must(t, err)
	if tk.ID != "t1" {
		t.Fatalf("hold #%s = %s", n, tk.ID)
	}
	st, err := a.AutomergeState()
	must(t, err)
	if !slices.Equal(st.Holds, []string{"t1"}) || st.Enabled {
		t.Fatalf("state = %+v", st)
	}
	gh := newSquashGH(t, a, origin)
	w := a.NewAutomerge(gh)
	must(t, w.SetEnabled(true))
	st, err = w.Check()
	must(t, err)
	if len(gh.order) != 0 || len(st.Stacks) != 1 || !st.Stacks[0].Held {
		t.Fatalf("held stack: merged %v, stacks %+v", gh.order, st.Stacks)
	}
	if _, err := a.AutomergeHold("t404"); err == nil {
		t.Fatal("hold of an unknown task: want error")
	}
}

// The at-risk flag covers its layer and the ones above, not those below.
func TestAutomergeFlagCovers(t *testing.T) {
	a := trainSetup(t)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	if why := a.flagCovers("t1"); why != "" {
		t.Fatalf("no flag: %q", why)
	}
	must(t, a.SetFlag(StackFlag{Task: "t2", Cause: "drifted", Acked: true}))
	if why := a.flagCovers("t1"); why != "" {
		t.Fatalf("t1 is below the flag: %q", why)
	}
	if why := a.flagCovers("t2"); !strings.Contains(why, "drifted") || !strings.Contains(why, "acknowledged") {
		t.Fatalf("t2: %q", why)
	}
}

// A held stack still restacks: stack rebase moves it onto the new main,
// reports what moved, and leaves the hold in place.
func TestStackRebaseMovesHeldStack(t *testing.T) {
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	origin, _ := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	_, err := a.PRs()
	must(t, err)
	_, err = a.AutomergeHold("t1")
	must(t, err)

	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	write(t, other, "elsewhere.txt", "x\n")
	commitAll(t, other, "someone else's work")
	git(t, other, "push", "-q", "origin", "main")
	main := git(t, other, "rev-parse", "HEAD")
	git(t, a.Root, "fetch", "-q", "origin")
	if n := a.behindBase(git(t, a.Root, "rev-parse", t1.Branch)); n != 1 {
		t.Fatalf("behind = %d, want 1", n)
	}

	res, err := a.RebaseStack("t1")
	must(t, err)
	if res.Stack != "t1" || len(res.Moves) == 0 || res.Moves[0].Task != "t1" {
		t.Fatalf("rebase = %+v", res)
	}
	if git(t, a.Root, "merge-base", main, t1.Branch) != main {
		t.Fatal("held stack wasn't moved onto main")
	}
	st, err := a.AutomergeState()
	must(t, err)
	if !slices.Equal(st.Holds, []string{"t1"}) {
		t.Fatalf("rebase dropped the hold: %+v", st.Holds)
	}
}

// A conflict stops stack rebase, moves nothing, and goes back to its owner.
func TestStackRebaseConflictReturnsToOwner(t *testing.T) {
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"README.md": "mine\n"})
	_, err := a.PRs()
	must(t, err)
	before := git(t, a.Root, "rev-parse", t1.Branch)
	_ = noticesFor(t, a, "t1")

	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	write(t, other, "README.md", "theirs\n")
	commitAll(t, other, "conflicting")
	git(t, other, "push", "-q", "origin", "main")

	_, err = a.RebaseStack("t1")
	var c *RestackConflict
	if !errors.As(err, &c) || c.Task != "t1" {
		t.Fatalf("err = %v, want a conflict owned by t1", err)
	}
	if git(t, a.Root, "rev-parse", t1.Branch) != before {
		t.Fatal("a conflicting rebase moved the branch")
	}
	ns := noticesFor(t, a, "t1")
	if len(ns) == 0 || ns[0].Kind != store.NoticeAction || !strings.Contains(ns[0].Text, "conflicts") {
		t.Fatalf("t1 notices = %+v", ns)
	}
	if _, err := a.RebaseStack("t404"); err == nil {
		t.Fatal("unknown stack: want error")
	}
}
