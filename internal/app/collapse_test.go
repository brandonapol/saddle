package app

import (
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/automerge"
)

// fakeHub is GitHub for collapse: each PR's head is its branch on origin,
// checks and labels are what the test sets, and a merge squashes the head's
// tree onto origin/main.
type fakeHub struct {
	t      *testing.T
	a      *App
	origin string
	base   map[string]string   // PR → base, as gh pr edit leaves it
	checks map[string][]string // PR → checks reported, one per read; the last repeats
	labels map[string][]string
	merged []string
}

func newFakeHub(t *testing.T, a *App, origin string) *fakeHub {
	return &fakeHub{t: t, a: a, origin: origin, base: map[string]string{}, checks: map[string][]string{}, labels: map[string][]string{}}
}

func (h *fakeHub) PR(url string) (automerge.PR, error) {
	ts, _ := h.a.Store.Tasks()
	pr := automerge.PR{URL: url, State: "OPEN", Mergeable: "MERGEABLE", MergeState: "CLEAN", Base: h.base[url], Labels: h.labels[url]}
	for _, t := range ts {
		if t.PR == url {
			pr.Head, pr.HeadSHA = t.Branch, remoteRev(h.t, h.origin, t.Branch)
		}
	}
	if slices.Contains(h.merged, url) {
		pr.State = "MERGED"
	}
	cs := h.checks[url]
	pr.Checks = automerge.ChecksNone
	if len(cs) > 0 {
		pr.Checks = cs[0]
		if len(cs) > 1 {
			h.checks[url] = cs[1:]
		}
	}
	return pr, nil
}

func (h *fakeHub) MergeMethod() (string, error) { return "squash", nil }

func (h *fakeHub) Merge(url, method, head string) error {
	git(h.t, h.a.Root, "fetch", "-q", "origin")
	c := git(h.t, h.a.Root, "commit-tree", head+"^{tree}", "-p", "origin/main", "-m", "squash "+url)
	git(h.t, h.a.Root, "push", "-q", "origin", c+":refs/heads/main")
	h.merged = append(h.merged, url)
	return nil
}

// stackOfTwo lands t1 and t2 as one two-layer stack with PRs: t2's PR
// targets t1's branch.
func stackOfTwo(t *testing.T) (*App, *fakeHub, func() []string) {
	t.Helper()
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	origin, ghLog := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "fix one's CI", map[string]string{"two.txt": "two\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	h := newFakeHub(t, a, origin)
	t1, _ := a.Store.Task("t1")
	t2, _ := a.Store.Task("t2")
	h.base[t1.PR], h.base[t2.PR] = "main", t1.Branch
	h.checks[t1.PR] = []string{automerge.ChecksFail}
	h.checks[t2.PR] = []string{automerge.ChecksPass}
	return a, h, ghLog
}

// #196: the bottom PR is red and its fix is in the PR above it. Collapse
// retargets the top PR to main, waits for its CI, squash-merges it, marks
// both tasks merged, closes the bottom PR with a comment, and restacks.
func TestCollapseRedBottomMergesTop(t *testing.T) {
	t.Parallel()
	a, h, ghLog := stackOfTwo(t)
	t1, _ := a.Store.Task("t1")
	t2, _ := a.Store.Task("t2")
	h.checks[t2.PR] = []string{automerge.ChecksPending, automerge.ChecksPass}

	res, err := a.CollapseStack("t1", h)
	if err != nil {
		t.Fatalf("collapse: %v", err)
	}
	if res.PR != t2.PR || !slices.Equal(res.Covered, []string{"t1", "t2"}) {
		t.Fatalf("collapse result = %+v", res)
	}
	if !slices.Equal(h.merged, []string{t2.PR}) {
		t.Fatalf("merged = %v, want only t2's PR", h.merged)
	}
	calls := ghLog()
	if !contains(calls, "pr edit "+t2.PR+" --base main") {
		t.Fatalf("t2's PR not retargeted to main: %v", calls)
	}
	low := prCalls(calls, t1.PR)
	if !slices.ContainsFunc(low, func(c string) bool { return strings.HasPrefix(c, "pr close") }) ||
		!slices.ContainsFunc(low, func(c string) bool { return strings.HasPrefix(c, "pr comment") && strings.Contains(c, t2.PR) }) {
		t.Fatalf("t1's PR wasn't closed with a comment: %v", low)
	}
	for _, id := range []string{"t1", "t2"} {
		if got := trainState(t, a, id); got != TrainMerged {
			t.Fatalf("%s = %s, want merged", id, got)
		}
	}
	for _, f := range []string{"one.txt", "two.txt"} {
		if _, err := trainGit(a.Root, "cat-file", "-e", "origin/main:"+f); err != nil {
			t.Fatalf("main lacks %s", f)
		}
	}
	if got, want := git(t, a.Root, "rev-parse", a.Cfg.Integration), git(t, a.Root, "rev-parse", "origin/main"); got != want {
		t.Fatal("integration wasn't restacked onto the new main")
	}
}

// A covered PR in an unrelated needs-human or conflict state stops collapse
// before anything changes.
func TestCollapseRefusesNeedsHuman(t *testing.T) {
	t.Parallel()
	a, h, ghLog := stackOfTwo(t)
	t1, _ := a.Store.Task("t1")
	h.labels[t1.PR] = []string{automerge.NeedsHuman}
	before := len(ghLog())
	if _, err := a.CollapseStack("t1", h); err == nil || !strings.Contains(err.Error(), automerge.NeedsHuman) {
		t.Fatalf("collapse over needs-human: %v", err)
	}
	for _, c := range ghLog()[before:] {
		if !strings.HasPrefix(c, "pr view") {
			t.Fatalf("collapse refused but still called gh %s", c)
		}
	}
	if len(h.merged) != 0 || trainState(t, a, "t1") != "landed" {
		t.Fatal("refused collapse changed something")
	}
}

// Red CI on the combined head puts the top PR back on its old base and
// merges nothing.
func TestCollapseRedCombinedHeadRestores(t *testing.T) {
	t.Parallel()
	a, h, ghLog := stackOfTwo(t)
	t1, _ := a.Store.Task("t1")
	t2, _ := a.Store.Task("t2")
	h.checks[t2.PR] = []string{automerge.ChecksPending, automerge.ChecksFail}
	if _, err := a.CollapseStack("t1", h); err == nil || !strings.Contains(err.Error(), "red") {
		t.Fatalf("collapse with red combined CI: %v", err)
	}
	if !contains(ghLog(), "pr edit "+t2.PR+" --base "+t1.Branch) {
		t.Fatalf("t2's PR not put back on %s: %v", t1.Branch, ghLog())
	}
	if len(h.merged) != 0 || trainState(t, a, "t2") != "landed" {
		t.Fatal("collapse merged despite red CI")
	}
}

// AutoCollapse acts only on a red bottom with a green PR above that holds
// its commits.
func TestAutoCollapse(t *testing.T) {
	t.Parallel()
	a, h, _ := stackOfTwo(t)
	t1, _ := a.Store.Task("t1")
	t2, _ := a.Store.Task("t2")
	h.checks[t1.PR] = []string{automerge.ChecksPass}
	if pr, err := a.AutoCollapse("t1", h); pr != "" || err != nil {
		t.Fatalf("green bottom collapsed: %q, %v", pr, err)
	}
	h.checks[t1.PR] = []string{automerge.ChecksFail}
	h.checks[t2.PR] = []string{automerge.ChecksPending}
	if pr, err := a.AutoCollapse("t1", h); pr != "" || err != nil {
		t.Fatalf("collapsed onto a pending PR: %q, %v", pr, err)
	}
	h.checks[t2.PR] = []string{automerge.ChecksPass}
	pr, err := a.AutoCollapse("t1", h)
	if err != nil || pr != t2.PR {
		t.Fatalf("auto collapse = %q, %v; want %s", pr, err, t2.PR)
	}
	if trainState(t, a, "t1") != TrainMerged {
		t.Fatal("t1 not marked merged")
	}
}
