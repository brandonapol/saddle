package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/ciwatch"
	"github.com/brandonapol/saddle/internal/store"
)

const (
	ciJob1 = "https://github.com/o/r/actions/runs/100/job/200"
	ciJob2 = "https://github.com/o/r/actions/runs/101/job/201"
)

// ciGH answers gh calls from a table keyed by the joined args.
type ciGH struct{ out map[string]string }

func newCIGH() *ciGH { return &ciGH{out: map[string]string{}} }

func (f *ciGH) run(_ context.Context, args ...string) (string, error) {
	return f.out[strings.Join(args, " ")], nil
}

// checks sets the one "ci / test" check gh reports for pr.
func (f *ciGH) checks(pr, bucket, link string) {
	f.out["pr checks "+pr+" --json name,workflow,bucket,link"] =
		fmt.Sprintf(`[{"name":"test","workflow":"ci","bucket":%q,"link":%q}]`, bucket, link)
	f.out["run view --job 200 --log-failed"] = "test\tRun go test\t2026-10-01T17:03:02.0529854Z --- FAIL: TestMeter\n"
	f.out["run view --job 201 --log-failed"] = "test\tRun go test\t2026-10-01T17:13:02.0529854Z --- FAIL: TestInvoice\n"
}

func ciTask(t *testing.T, a *App, id, status, pr string) store.Task {
	t.Helper()
	tk := store.Task{ID: id, Title: "billing meter", Role: store.RoleWorker, Branch: "saddle/" + id + "-billing-meter", Status: status, PR: pr}
	must(t, a.Store.CreateTask(tk))
	return tk
}

func newCI(t *testing.T, a *App, gh *ciGH) *CIWatcher {
	t.Helper()
	c, err := a.NewCIWatcher(gh.run)
	must(t, err)
	return c
}

func notices(t *testing.T, a *App, task string) []store.Notice {
	t.Helper()
	ns, err := a.Store.TakeNotices(task, false)
	must(t, err)
	return ns
}

func onlyNotice(t *testing.T, a *App, task, kind string, want ...string) store.Notice {
	t.Helper()
	ns := notices(t, a, task)
	if len(ns) != 1 {
		t.Fatalf("%s got %d notices, want 1: %+v", task, len(ns), ns)
	}
	if ns[0].Kind != kind {
		t.Errorf("%s notice kind %q, want %q", task, ns[0].Kind, kind)
	}
	for _, w := range want {
		if !strings.Contains(ns[0].Text, w) {
			t.Errorf("%s notice missing %q:\n%s", task, w, ns[0].Text)
		}
	}
	return ns[0]
}

func TestCITargetsAreLiveTasksWithPRs(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	ciTask(t, a, "t1", store.Landed, "https://github.com/o/r/pull/1")
	ciTask(t, a, "t2", store.Done, "https://github.com/o/r/pull/2")
	ciTask(t, a, "t3", store.Killed, "https://github.com/o/r/pull/3")
	ciTask(t, a, "t4", store.Running, "")

	ts, err := a.CITargets(context.Background())
	must(t, err)
	want := []ciwatch.Target{
		{Task: "t1", Branch: "saddle/t1-billing-meter", PR: "https://github.com/o/r/pull/1"},
		{Task: "t2", Branch: "saddle/t2-billing-meter", PR: "https://github.com/o/r/pull/2"},
	}
	if fmt.Sprint(ts) != fmt.Sprint(want) {
		t.Fatalf("targets %+v, want %+v", ts, want)
	}
}

func TestCIFailureGoesToActiveOwnerAndOrchestrator(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	ciTask(t, a, "t2", store.Done, "https://github.com/o/r/pull/2")
	gh := newCIGH()
	gh.checks("https://github.com/o/r/pull/2", "fail", ciJob1)

	newCI(t, a, gh).Poll(context.Background())

	onlyNotice(t, a, "t2", store.NoticeAction, `"ci / test"`, "--- FAIL: TestMeter", "Fix it on saddle/t2-billing-meter")
	onlyNotice(t, a, OrchestratorID, store.NoticeAction, "t2", "--- FAIL: TestMeter", "t2 was told to fix it")
	tasks, err := a.Store.Tasks()
	must(t, err)
	if len(tasks) != 1 {
		t.Fatalf("an active owner should fix its own CI, but tasks are %+v", tasks)
	}
}

func TestCIFailureOnLandedTaskSpawnsOneFixTask(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	ciTask(t, a, "t1", store.Landed, "https://github.com/o/r/pull/1")
	gh := newCIGH()
	gh.checks("https://github.com/o/r/pull/1", "fail", ciJob1)
	c := newCI(t, a, gh)

	c.Poll(context.Background())

	fix, err := a.Store.Task("t2")
	if err != nil {
		t.Fatalf("no fix task spawned: %v", err)
	}
	if fix.Parent != OrchestratorID || !strings.Contains(fix.Title, "t1") || !strings.Contains(fix.Prompt, "--- FAIL: TestMeter") {
		t.Errorf("fix task: %+v", fix)
	}
	if strings.Contains(fix.Prompt, "Fix it on saddle/t1") {
		t.Errorf("fix prompt sends the fix to the landed branch:\n%s", fix.Prompt)
	}
	if len(notices(t, a, "t1")) != 0 {
		t.Error("a landed task was notified")
	}
	onlyNotice(t, a, OrchestratorID, store.NoticeAction, "t1", "--- FAIL: TestMeter", "spawned t2 to fix it")

	// A new run failing again goes to the fix task already on it.
	gh.checks("https://github.com/o/r/pull/1", "fail", ciJob2)
	c.Poll(context.Background())
	if _, err := a.Store.Task("t3"); err == nil {
		t.Fatal("a second fix task was spawned for the same PR")
	}
	onlyNotice(t, a, "t2", store.NoticeAction, "--- FAIL: TestInvoice")
	onlyNotice(t, a, OrchestratorID, store.NoticeAction, "t2 is already fixing it")
}

func TestCIFailureOffersFixWhenSpawnFails(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.Cfg.Concurrency = 0
	ciTask(t, a, "t1", store.Landed, "https://github.com/o/r/pull/1")
	gh := newCIGH()
	gh.checks("https://github.com/o/r/pull/1", "fail", ciJob1)

	newCI(t, a, gh).Poll(context.Background())

	if _, err := a.Store.Task("t2"); err == nil {
		t.Fatal("spawned past the concurrency cap")
	}
	onlyNotice(t, a, OrchestratorID, store.NoticeAction, "--- FAIL: TestMeter", "concurrency cap", "spawn")
}

func TestCIRecoveryIsInfo(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	ciTask(t, a, "t2", store.Done, "https://github.com/o/r/pull/2")
	gh := newCIGH()
	gh.checks("https://github.com/o/r/pull/2", "fail", ciJob1)
	c := newCI(t, a, gh)
	c.Poll(context.Background())
	notices(t, a, "t2")
	notices(t, a, OrchestratorID)

	gh.checks("https://github.com/o/r/pull/2", "pass", ciJob2)
	c.Poll(context.Background())

	onlyNotice(t, a, "t2", store.NoticeInfo, "passing again")
	// The orchestrator gets it in the digest, not as a notice (#222).
	if ns := notices(t, a, OrchestratorID); len(ns) != 0 {
		t.Fatalf("orchestrator notices = %+v, want none", ns)
	}
	if !hasEvent(t, a, EventNoticeDigest, "passing again") {
		t.Fatal("no notice_digest event for the recovery")
	}
}

func TestCIStateSurvivesRestart(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	ciTask(t, a, "t2", store.Done, "https://github.com/o/r/pull/2")
	gh := newCIGH()
	gh.checks("https://github.com/o/r/pull/2", "fail", ciJob1)
	newCI(t, a, gh).Poll(context.Background())
	notices(t, a, "t2")
	notices(t, a, OrchestratorID)

	// A new watcher, as after saddle up restarts, sees the same failure.
	newCI(t, a, gh).Poll(context.Background())

	if ns := notices(t, a, "t2"); len(ns) != 0 {
		t.Fatalf("restart re-reported a known failure: %+v", ns)
	}
	if ns := notices(t, a, OrchestratorID); len(ns) != 0 {
		t.Fatalf("restart re-reported a known failure to the orchestrator: %+v", ns)
	}
}

// ciMakeFail makes pr's check fail with make's missing-target error.
func (f *ciGH) ciMakeFail(pr, link, target string) {
	f.checks(pr, "fail", link)
	f.out["run view --job 200 --log-failed"] = "e2e\tRun make test/e2e\t2026-10-03T17:03:02.0529854Z make: *** No rule to make target '" + target + "'.  Stop.\n"
}

// #193: a landed task's PR failed with "No rule to make target test/e2e"
// because t1, which adds it, hasn't merged. No fix task spawns; an event and
// a digest line say why.
func TestCIFailureExplainedBySiblingSpawnsNoFix(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	landTask(t, a, "t1", "e2e harness", map[string]string{"Makefile": "test/e2e:\n\tgo test ./e2e\n"})
	ciTask(t, a, "t2", store.Landed, "https://github.com/o/r/pull/2")
	gh := newCIGH()
	gh.ciMakeFail("https://github.com/o/r/pull/2", ciJob1, "test/e2e")

	newCI(t, a, gh).Poll(context.Background())

	if _, err := a.Store.Task("t3"); err == nil {
		t.Fatal("spawned a fix task for a failure t1 explains")
	}
	if !hasEvent(t, a, EventCIExplained, "t1 adds make target test/e2e") {
		t.Fatal("no ci_explained event naming t1")
	}
	if !hasEvent(t, a, EventNoticeDigest, "no fix task was spawned") {
		t.Fatal("the orchestrator wasn't told why no fix spawned")
	}
}

// A sibling that doesn't add the missing target explains nothing.
func TestCIFailureNotExplainedBySiblingSpawnsFix(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	landTask(t, a, "t1", "e2e harness", map[string]string{"Makefile": "test/e2e:\n\tgo test ./e2e\n"})
	ciTask(t, a, "t2", store.Landed, "https://github.com/o/r/pull/2")
	gh := newCIGH()
	gh.ciMakeFail("https://github.com/o/r/pull/2", ciJob1, "lint")

	newCI(t, a, gh).Poll(context.Background())

	if _, err := a.Store.Task("t3"); err != nil {
		t.Fatalf("no fix task for a failure no sibling explains: %v", err)
	}
}

// A stacked task's red CI belongs to the ci-red watcher: ciwatch neither
// spawns a fix nor reports it.
func TestCIFailureOnStackedTaskDefersToCIRed(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	landTask(t, a, "t1", "billing", map[string]string{"meter.go": "package m\n"})
	must(t, a.Store.SetField("t1", "pr", "https://github.com/o/r/pull/1"))
	notices(t, a, OrchestratorID)
	gh := newCIGH()
	gh.checks("https://github.com/o/r/pull/1", "fail", ciJob1)

	newCI(t, a, gh).Poll(context.Background())

	if _, err := a.Store.Task("t2"); err == nil {
		t.Fatal("ciwatch spawned a fix for a ci-red layer")
	}
	if hasEvent(t, a, "ci_failed", "") {
		t.Fatal("ciwatch handled a ci-red layer's failure")
	}
}
