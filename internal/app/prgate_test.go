package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// gateStack lands four layers in one linear stack. t3 adds a file the check
// rejects unless t4's file is there too, so t3's own tip is red while t4's
// (and the integration tip) is green: the cspell case from #223.
func gateStack(t *testing.T) (*App, string, []store.Task) {
	t.Helper()
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	origin, _ := originWithGh(t, a)
	var ts []store.Task
	for _, c := range []struct{ id, file string }{{"t1", "one.txt"}, {"t2", "two.txt"}, {"t3", "uses-word.txt"}, {"t4", "allow-word.txt"}} {
		ts = append(ts, landTask(t, a, c.id, c.id+" work", map[string]string{c.file: c.id + "\n"}))
	}
	a.Cfg.Train.Prepublish.Cmd = "test ! -f uses-word.txt || test -f allow-word.txt || { echo 'Unknown word (uploaders)'; exit 1; }"
	return a, origin, ts
}

// #223: a layer whose own tip fails a check is never published, nor is
// anything above it; the layers below go out, and the error names the layer,
// the check, its command and its output.
func TestPRsGateHoldsRedLayerAndAbove(t *testing.T) {
	t.Parallel()
	a, origin, ts := gateStack(t)
	_, err := a.PRs()
	if err == nil {
		t.Fatal("prs published a stack whose layer t3 is red at its own tip")
	}
	for _, want := range []string{"layer t3", "prepublish check", "uses-word.txt", "Unknown word (uploaders)", "(t4)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	for i, tk := range ts {
		got, _ := a.Store.Task(tk.ID)
		pushed := remoteRev(t, origin, tk.Branch) != ""
		if want := i < 2; pushed != want || (got.PR != "") != want {
			t.Errorf("%s: pushed %v, PR %q; want published %v", tk.ID, pushed, got.PR, want)
		}
	}
	s, err := a.Gate()
	must(t, err)
	if len(s.Red) != 1 || s.Red[0].Task != "t3" || s.Red[0].Check.Name != "prepublish" || !slices.Equal(s.Red[0].Held, []string{"t4"}) || s.Red[0].Source != GateSourcePRs {
		t.Fatalf("gate state = %+v, want t3 red holding t4", s.Red)
	}

	// The escape hatch: with the gate off, everything goes out and the red
	// record is dropped.
	a.Cfg.Train.Prepublish.Off = true
	if _, err := a.PRs(); err != nil {
		t.Fatalf("prs with the gate off: %v", err)
	}
	for _, tk := range ts {
		if remoteRev(t, origin, tk.Branch) == "" {
			t.Errorf("%s not pushed with the gate off", tk.ID)
		}
	}
	if s, _ := a.Gate(); len(s.Red) != 0 {
		t.Fatalf("red records survive the gate going off: %+v", s.Red)
	}
}

// A green layer is checked once per tree: prs again over the same heads runs
// nothing, and a red layer that is fixed clears its record.
func TestPRsGateCachesGreenAndClearsFixed(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	_, _ = originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	count := filepath.Join(t.TempDir(), "runs")
	a.Cfg.Train.Prepublish.Cmd = "echo run >> " + count
	runs := func() int {
		b, _ := os.ReadFile(count)
		return strings.Count(string(b), "run")
	}
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	if n := runs(); n != 2 {
		t.Fatalf("first prs ran the check %d times, want once per layer", n)
	}
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	if n := runs(); n != 2 {
		t.Fatalf("second prs over the same heads ran the check again (%d runs)", n)
	}

	must(t, a.recordGate([]GateRed{{Task: "t2", Head: "abc", Check: GateCheck{Name: "prepublish", Cmd: "x"}}}, nil, GateSourceRestack))
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	if s, _ := a.Gate(); len(s.Red) != 0 {
		t.Fatalf("t2 passed the gate but its old red record stays: %+v", s.Red)
	}
}

// Parallel workers check every layer and still hold the red one.
func TestPRsGateParallel(t *testing.T) {
	t.Parallel()
	a, origin, ts := gateStack(t)
	a.Cfg.Train.Prepublish.Parallel = 3
	if _, err := a.PRs(); err == nil || !strings.Contains(err.Error(), "layer t3") {
		t.Fatalf("parallel gate: err = %v", err)
	}
	if remoteRev(t, origin, ts[1].Branch) == "" || remoteRev(t, origin, ts[3].Branch) != "" {
		t.Fatal("parallel gate published the wrong layers")
	}
}

func TestGateChecksDedupesAndSkipsNone(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Train.Lint.Set, a.Cfg.Train.Lint.Cmd = true, "make check"
	a.Cfg.Test.Cmd = "make check"
	a.Cfg.Train.Prepublish.Cmd = "make check/spelling"
	got := a.GateChecks(false)
	if len(got) != 2 || got[0] != (GateCheck{"lint", "make check"}) || got[1] != (GateCheck{"prepublish", "make check/spelling"}) {
		t.Fatalf("checks = %+v", got)
	}
	if got := a.GateChecks(true); len(got) != 1 || got[0].Name != "prepublish" {
		t.Fatalf("cheap checks = %+v", got)
	}
	a.Cfg.Test.Cmd, a.Cfg.Train.Lint.Cmd = NoTestCmd, ""
	if got := a.GateChecks(false); len(got) != 1 {
		t.Fatalf("none and \"\" should not be checks: %+v", got)
	}
	a.Cfg.Train.Prepublish.Off = true
	if got := a.GateChecks(false); got != nil {
		t.Fatalf("gate off still checks: %+v", got)
	}
}

// A check that runs past the timeout is killed, with everything it started,
// and counts as red, not as the environment.
func TestGateLayerTimesOut(t *testing.T) {
	t.Parallel()
	a, _, ts := gateStack(t)
	a.Cfg.Train.Prepublish.Timeout = 200 * time.Millisecond
	dir, err := a.gateWorktree(0)
	must(t, err)
	head := git(t, a.Root, "rev-parse", a.Cfg.Integration)
	start := time.Now()
	var mu sync.Mutex
	r, _, err := a.gateLayer(dir, gateJob{task: ts[0].ID, head: head}, []GateCheck{{Name: "prepublish", Cmd: "sleep 30 & sleep 30; wait"}}, map[string]time.Time{}, &mu)
	must(t, err)
	if r == nil || !r.timedOut || r.env != nil || !strings.Contains(r.out, "timed out") {
		t.Fatalf("timeout: %+v", r)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("the timed-out check took %v to stop", d)
	}
}

// #274: a check that fails once on the environment (disk quota exceeded)
// is retried, as the train's gate is, and the layer publishes.
func TestPRsGateRetriesEnvironmentFailure(t *testing.T) {
	t.Parallel()
	a, origin, ts := gateStack(t)
	once := filepath.Join(t.TempDir(), "once")
	a.Cfg.Train.Prepublish.Cmd = "if [ ! -e " + once + " ]; then touch " + once + "; echo 'compile: writing output: disk quota exceeded'; exit 1; fi"
	if _, err := a.PRs(); err != nil {
		t.Fatalf("prs: %v", err)
	}
	for _, tk := range ts {
		if remoteRev(t, origin, tk.Branch) == "" {
			t.Errorf("%s not pushed", tk.ID)
		}
	}
	if s, _ := a.Gate(); len(s.Red) != 0 {
		t.Fatalf("red records: %+v", s.Red)
	}
}

// #274: a check that keeps failing on the environment holds the layer and
// the ones above it, but reports the environment, not the layer: no red
// record, so no ci-red hold or repair for it.
func TestPRsGateEnvironmentFailureDoesNotBlameTheLayer(t *testing.T) {
	t.Parallel()
	a, origin, ts := gateStack(t)
	a.Cfg.Train.Prepublish.Cmd = "test ! -f uses-word.txt || { echo 'sqlite: disk I/O error'; echo 'disk quota exceeded'; exit 1; }"
	_, err := a.PRs()
	if err == nil {
		t.Fatal("prs published layers whose check could not run")
	}
	for _, want := range []string{"pre-publish gate hit the environment", "disk quota exceeded", "not the layer", "t3"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "fails its") {
		t.Errorf("error blames the layer:\n%v", err)
	}
	for i, tk := range ts {
		if pushed, want := remoteRev(t, origin, tk.Branch) != "", i < 2; pushed != want {
			t.Errorf("%s: pushed %v, want %v", tk.ID, pushed, want)
		}
	}
	if s, _ := a.Gate(); len(s.Red) != 0 {
		t.Fatalf("the environment marked a layer red: %+v", s.Red)
	}
	es, err := a.Store.Events(-1)
	must(t, err)
	retries := 0
	for _, e := range es {
		if e.Kind == "prepublish_red" {
			t.Fatalf("layer flagged red: %+v", e)
		}
		if e.Kind == EventGateEnv && e.Task == "t3" {
			retries++
		}
	}
	if retries == 0 {
		t.Fatal("no gate_env events for t3")
	}
}

// saddle publish runs the gate on the head it is about to push.
func TestPublishRefusesRedHead(t *testing.T) {
	t.Parallel()
	a, origin, _ := publishSetup(t)
	landTask(t, a, "t1", "one", map[string]string{"bad.txt": "one\n"})
	a.Cfg.Train.Prepublish.Cmd = "test ! -f bad.txt"
	_, err := a.Publish(PublishReq{Target: "t1", BranchName: "fix/one"})
	if err == nil || !strings.Contains(err.Error(), "pre-publish gate") || !strings.Contains(err.Error(), "test ! -f bad.txt") {
		t.Fatalf("publish of a red head: err = %v", err)
	}
	if remoteRev(t, origin, "fix/one") != "" {
		t.Fatal("publish pushed a red head")
	}
}

// #223 item 2: restack re-runs the cheap checks on every layer it re-cut,
// records a red one so prs holds it, and tells the orchestrator.
func TestGateRestackFlagsRecutLayer(t *testing.T) {
	t.Parallel()
	a, _, ts := gateStack(t)
	_, _ = a.Store.TakeNotices(OrchestratorID, false)
	t3, t4 := ts[2], ts[3]
	plan := []restacked{
		{landedTask: landedTask{Task: t3, To: "old3"}, NewFrom: git(t, a.Root, "rev-parse", a.Cfg.Integration+"~2"), NewTo: git(t, a.Root, "rev-parse", a.Cfg.Integration+"~1")},
		{landedTask: landedTask{Task: t4, To: git(t, a.Root, "rev-parse", a.Cfg.Integration)}, NewFrom: "x", NewTo: git(t, a.Root, "rev-parse", a.Cfg.Integration)},
	}
	reds := a.gateRestack(plan)
	if len(reds) != 1 || reds[0].Task != "t3" {
		t.Fatalf("restack gate = %+v, want t3 red", reds)
	}
	s, err := a.Gate()
	must(t, err)
	if len(s.Red) != 1 || s.Red[0].Source != GateSourceRestack {
		t.Fatalf("gate state = %+v", s.Red)
	}
	ev, err := a.Store.Events(200)
	must(t, err)
	if !slices.ContainsFunc(ev, func(e store.Event) bool { return e.Task == "t3" && e.Kind == "prepublish_red" }) {
		t.Fatal("no prepublish_red event for t3")
	}
	ns, err := a.Store.PeekNotices(OrchestratorID, false)
	must(t, err)
	i := slices.IndexFunc(ns, func(n store.Notice) bool { return strings.Contains(n.Text, "Restack re-cut t3") })
	if i < 0 {
		t.Fatalf("orchestrator wasn't told about t3: %+v", ns)
	}
	if class, _ := ClassifyNotice(ns[i].Kind, ns[i].Text, false); class != NoticeInterrupt {
		t.Fatalf("a red re-cut layer is %s, want an interrupt", class)
	}
}

// #223 item 2 through Restack: main gains migration 0002 while t2, stacked,
// adds its own 0002. Restack re-cuts t2 onto main cleanly, but its new tip
// has two 0002 migrations: the cheap check flags t2, and prs then holds t2
// and t3 above it while t1 goes out.
func TestRestackFlagsDuplicateMigration(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	origin, _ := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"migrations/0002_two.sql": "two\n"})
	landTask(t, a, "t3", "three", map[string]string{"three.txt": "three\n"})
	a.Cfg.Train.Prepublish.Cmd = `d=$(ls migrations 2>/dev/null | cut -c1-4 | sort | uniq -d); [ -z "$d" ] || { echo "duplicate migration $d"; exit 1; }`

	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	write(t, other, "migrations/0002_main.sql", "main\n")
	git(t, other, "add", "-A")
	git(t, other, "commit", "-qm", "main migration")
	git(t, other, "push", "-q", "origin", "main")

	res, err := a.Restack()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(res.GateRed, []string{"t2"}) {
		t.Fatalf("restack gate red = %v, want [t2]", res.GateRed)
	}
	_, err = a.PRs()
	if err == nil || !strings.Contains(err.Error(), "layer t2") || !strings.Contains(err.Error(), "duplicate migration 0002") {
		t.Fatalf("prs after a red restack: %v", err)
	}
	t1, _ := a.Store.Task("t1")
	t3, _ := a.Store.Task("t3")
	if t1.PR == "" || t3.PR != "" {
		t.Fatalf("t1 PR %q, t3 PR %q: want t1 published and t3 held", t1.PR, t3.PR)
	}
}

// The train just ran [test] cmd on each tree it landed; prs over those same
// heads doesn't run it again.
func TestPRsGateTrustsTheTrainsTests(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	_, _ = originWithGh(t, a)
	count := filepath.Join(t.TempDir(), "runs")
	a.Cfg.Test.Cmd = "echo run >> " + count
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	b, _ := os.ReadFile(count)
	landed := strings.Count(string(b), "run")
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(count); strings.Count(string(b), "run") != landed {
		t.Fatalf("prs re-ran the tests the train just passed on the same trees: %d runs, %d at land", strings.Count(string(b), "run"), landed)
	}
}
