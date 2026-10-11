package app

import (
	"context"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// fakeToken is a GitHub-token-shaped literal, built at runtime so no commit
// of saddle holds a whole one (see internal/secretscan).
func fakeToken() string { return "gh" + "p_" + strings.Repeat("a1B2", 9) }

// #405: the t150 shape. Commit 1 adds a secret-shaped fixture, commit 2
// rewrites it as runtime concatenation. The tree is clean, but a history
// scanner reads every commit, so the land gate rejects the branch and says
// which commit and line to squash.
func TestLandRejectsSecretInAnyCommit(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	tk := queueTask(t, a, "t1", "scrub",
		map[string]string{"internal/remote/scrub_test.go": "package remote\n\nvar tok = \"" + fakeToken() + "\"\n"},
		map[string]string{"internal/remote/scrub_test.go": "package remote\n\nvar tok = \"gh\" + \"p_x\"\n"})
	first := git(t, tk.Worktree, "rev-parse", "HEAD~1")
	before := git(t, a.Root, "rev-parse", a.Cfg.Integration)

	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TestFailed {
		t.Fatalf("land = %+v, want test_failed", rs)
	}
	if got := git(t, a.Root, "rev-parse", a.Cfg.Integration); got != before {
		t.Fatalf("integration moved to %s", got)
	}
	ns := notices(t, a, "t1")
	want := "commit " + short(first) + " adds a secret-shaped literal at internal/remote/scrub_test.go:3; build it at runtime and squash"
	if len(ns) != 1 || !strings.Contains(ns[0].Text, want) {
		t.Fatalf("notices = %+v, want %q", ns, want)
	}
	if strings.Contains(ns[0].Text, fakeToken()) {
		t.Fatalf("the notice repeats the secret: %s", ns[0].Text)
	}
}

// A literal marked gitleaks:allow is the escape hatch for a false positive.
func TestScanCommitsHonoursAllowPragma(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	base := git(t, a.Root, "rev-parse", "HEAD")
	write(t, a.Root, "x.go", "package x\n\nvar s = \""+fakeToken()+"\" // gitleaks"+":allow\n")
	commitAll(t, a.Root, "allowed")
	hits, err := scanCommits(a.Root, base, "HEAD")
	must(t, err)
	if len(hits) != 0 {
		t.Fatalf("hits = %+v", hits)
	}
}

func TestIsHistoryCheck(t *testing.T) {
	t.Parallel()
	for _, c := range []string{"GitGuardian Security Checks", "ci / gitleaks", "TruffleHog OSS", "DCO", "commitlint / lint", "Semantic commit-message"} {
		if !isHistoryCheck(c, nil) {
			t.Errorf("%q is a history check", c)
		}
	}
	for _, c := range []string{"CI / test", "ci / lint", "deploy-docs"} {
		if isHistoryCheck(c, nil) {
			t.Errorf("%q is not a history check", c)
		}
	}
	if !isHistoryCheck("Corp / secret-sweep", []string{"secret-sweep"}) {
		t.Error("[ci] history_checks extends the list")
	}
}

// landBad lands t1 the way t150 landed before the gate existed (two commits:
// a secret, then its runtime rewrite) and t2 above it, and publishes PRs.
func landBad(t *testing.T) (*App, string) {
	t.Helper()
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	origin, _ := originWithGh(t, a)
	a.Cfg.CI.NoSecretGate = true
	landTask(t, a, "t1", "scrub",
		map[string]string{"scrub_test.go": "package x\n\nvar tok = \"" + fakeToken() + "\"\n"},
		map[string]string{"scrub_test.go": "package x\n\nvar tok = \"gh\" + \"p_x\"\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	a.Cfg.CI.NoSecretGate = false
	_, err := a.PRs()
	must(t, err)
	_, _ = a.Store.TakeNotices(OrchestratorID, false)
	return a, origin
}

func layerOf(t *testing.T, a *App, id string) landedTask {
	t.Helper()
	st, err := a.landedStack()
	must(t, err)
	for _, l := range st {
		if l.ID == id {
			return l
		}
	}
	t.Fatalf("%s not in the stack", id)
	return landedTask{}
}

// #405: a red history check on an unmerged saddle layer rewrites the layer
// (one squashed commit, same tree), restacks what is above it and pushes,
// instead of spawning a "Fix CI" task that adds commits.
func TestHistoryCheckRedRewritesLayer(t *testing.T) {
	t.Parallel()
	a, origin := landBad(t)
	l1 := layerOf(t, a, "t1")
	tree := git(t, a.Root, "rev-parse", l1.To+"^{tree}")
	top := git(t, a.Root, "rev-parse", a.Cfg.Integration+"^{tree}")
	tasks, _ := a.Store.Tasks()
	must(t, a.SetCIRed(CIRedState{Red: []CIRedLayer{{Task: "t1", PR: l1.PR, Head: l1.To, Checks: []string{"GitGuardian Security Checks"}}}}))

	fails := []CIRedFailure{{Check: "GitGuardian Security Checks", RunURL: "https://dashboard.gitguardian.com/x"}}
	note, handled, err := a.HistoryRepair("t1", fails)
	must(t, err)
	if !handled || !strings.Contains(note, "rewrote") {
		t.Fatalf("handled = %v, note = %q", handled, note)
	}
	if after, _ := a.Store.Tasks(); len(after) != len(tasks) {
		t.Fatalf("a task was spawned: %+v", after)
	}
	n1 := layerOf(t, a, "t1")
	if n := git(t, a.Root, "rev-list", "--count", n1.From+".."+n1.To); n != "1" {
		t.Fatalf("t1's layer has %s commits, want 1 squashed", n)
	}
	if n1.From != l1.From || git(t, a.Root, "rev-parse", n1.To+"^{tree}") != tree {
		t.Fatal("the rewrite changed t1's base or tree")
	}
	if hits, _ := scanCommits(a.Root, n1.From, n1.To); len(hits) != 0 {
		t.Fatalf("the rewritten layer still has %+v", hits)
	}
	if got := git(t, a.Root, "rev-parse", a.Cfg.Integration+"^{tree}"); got != top {
		t.Fatal("the rewrite changed integration's tree")
	}
	if got := remoteRev(t, origin, n1.Branch); got != n1.To {
		t.Fatalf("origin has %s at %s, want %s", n1.Branch, got, n1.To)
	}
	n2 := layerOf(t, a, "t2")
	if n2.From != n1.To {
		t.Fatalf("t2 starts at %s, want t1's new head %s", n2.From, n1.To)
	}

	// Red again on the rewritten head: no second rewrite, one decision card.
	s, _ := a.CIRed()
	s.Red[0].Head = n1.To
	must(t, a.SetCIRed(s))
	_, handled, err = a.HistoryRepair("t1", fails)
	must(t, err)
	ns := notices(t, a, OrchestratorID)
	if !handled || len(ns) != 1 || !strings.Contains(ns[0].Text, "dismiss in dashboard: https://dashboard.gitguardian.com/x") {
		t.Fatalf("handled = %v, notices = %+v", handled, ns)
	}
	if _, handled, _ = a.HistoryRepair("t1", fails); !handled || len(notices(t, a, OrchestratorID)) != 0 {
		t.Fatal("the card was raised twice")
	}
}

// A history check that isn't one, or a red layer whose tree still holds the
// literal, is left to the ordinary repair.
func TestHistoryRepairLeavesOtherFailures(t *testing.T) {
	t.Parallel()
	a, _ := landBad(t)
	l1 := layerOf(t, a, "t1")
	must(t, a.SetCIRed(CIRedState{Red: []CIRedLayer{{Task: "t1", PR: l1.PR, Head: l1.To, Checks: []string{"CI / test"}}}}))
	if _, handled, err := a.HistoryRepair("t1", []CIRedFailure{{Check: "CI / test"}}); err != nil || handled {
		t.Fatalf("a test failure was handled: %v %v", handled, err)
	}
}

// An ignore file a repair added proves nothing while the check stays red.
func TestHistoryRepairSaysIgnoreFileIsNoFix(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	_, _ = originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"}, map[string]string{".gitguardian.yaml": "version: 2\n"})
	_, err := a.PRs()
	must(t, err)
	l1 := layerOf(t, a, "t1")
	must(t, a.SetCIRed(CIRedState{Red: []CIRedLayer{{Task: "t1", PR: l1.PR, Head: l1.To, Checks: []string{"GitGuardian Security Checks"}}}}))
	note, _, err := a.HistoryRepair("t1", []CIRedFailure{{Check: "GitGuardian Security Checks"}})
	must(t, err)
	if !strings.Contains(note, ".gitguardian.yaml") || !strings.Contains(note, "isn't a fix") {
		t.Fatalf("note = %q", note)
	}
}

// #405: a fix task for a fix task is refused with a reason, by title and by
// lineage.
func TestRepairOfRepairRefused(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	_, err := a.Spawn(SpawnReq{Title: "Fix CI for t154 Fix CI for t150 remote control"})
	if err == nil || !strings.Contains(err.Error(), "repair of a repair") {
		t.Fatalf("by title: err = %v", err)
	}
	if ts, _ := a.Store.Tasks(); len(ts) != 0 {
		t.Fatalf("refused spawn left %+v", ts)
	}

	// By lineage: t2 repaired t1 (under any title), and now t2 is red.
	must(t, a.SetCIRed(CIRedState{
		Red:     []CIRedLayer{{Task: "t2", PR: "https://github.com/o/r/pull/2", Head: "abc", Checks: []string{"CI / test"}}},
		Repairs: []CIRepairRecord{{Task: "t1", Head: "def", Repair: "t2", State: ciRepairFolded}},
	}))
	note, handled, err := a.HistoryRepair("t2", []CIRedFailure{{Check: "CI / test"}})
	must(t, err)
	if !handled || !strings.Contains(note, "repair of a repair") {
		t.Fatalf("by lineage: handled = %v, note = %q", handled, note)
	}
	ns := notices(t, a, OrchestratorID)
	if len(ns) != 1 || !strings.Contains(ns[0].Text, "t1") {
		t.Fatalf("notices = %+v", ns)
	}
}

// The CI watcher refuses too: a landed fix task's own red PR escalates
// instead of spawning "Fix CI for Fix CI for …".
func TestCIFailureOnFixTaskEscalates(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	must(t, a.Store.CreateTask(store.Task{ID: "t1", Title: "Fix CI for t0 something", Role: store.RoleWorker,
		Branch: "saddle/t1-fix", Status: store.Landed, PR: "https://github.com/o/r/pull/1"}))
	gh := newCIGH()
	gh.checks("https://github.com/o/r/pull/1", "fail", ciJob1)
	c := newCI(t, a, gh)
	c.Poll(context.Background())
	if _, err := a.Store.Task("t2"); err == nil {
		t.Fatal("a fix task for a fix task was spawned")
	}
	onlyNotice(t, a, OrchestratorID, store.NoticeAction, "repair of a repair")
}
