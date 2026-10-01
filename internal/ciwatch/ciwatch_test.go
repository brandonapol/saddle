package ciwatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	jobLink  = "https://github.com/o/r/actions/runs/100/job/200"
	jobLink2 = "https://github.com/o/r/actions/runs/101/job/201"
	runURL   = "https://github.com/o/r/actions/runs/100"
)

// fakeGH answers gh calls from a table keyed by the joined args and records
// every call.
type fakeGH struct {
	out   map[string]string
	errs  map[string]error
	calls []string
}

func newFake() *fakeGH {
	return &fakeGH{out: map[string]string{}, errs: map[string]error{}}
}

func (f *fakeGH) run(_ context.Context, args ...string) (string, error) {
	k := strings.Join(args, " ")
	f.calls = append(f.calls, k)
	return f.out[k], f.errs[k]
}

func (f *fakeGH) checks(ref, json string) {
	f.out["pr checks "+ref+" --json name,workflow,bucket,link"] = json
}

func (f *fakeGH) count(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func check(bucket, link string) string {
	return fmt.Sprintf(`[{"name":"test","workflow":"ci","bucket":%q,"link":%q},`+
		`{"name":"GitGuardian","workflow":"","bucket":"pass","link":"https://dashboard.gitguardian.com"}]`, bucket, link)
}

func newWatcher(t *testing.T, gh *fakeGH, targets ...Target) (*Watcher, *[]error) {
	t.Helper()
	var errs []error
	w, err := New(Config{
		GH:      gh.run,
		Targets: func(context.Context) ([]Target, error) { return targets, nil },
		OnError: func(_ Target, err error) { errs = append(errs, err) },
		Now:     func() time.Time { return time.Unix(0, 0) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return w, &errs
}

var t4 = Target{Task: "t4", Branch: "saddle/t4-ci", PR: "60"}

func TestNewFailure(t *testing.T) {
	gh := newFake()
	gh.checks("60", check("fail", jobLink))
	gh.out["run view --job 200 --log-failed"] = "" +
		"test\tSet up job\t2026-10-01T17:03:01.0000000Z starting\n" +
		"test\tRun go test\t2026-10-01T17:03:02.0529854Z --- FAIL: TestX\n" +
		"test\tRun go test\t2026-10-01T17:03:02.0539712Z ##[error]Process completed with exit code 1.\n"
	w, errs := newWatcher(t, gh, t4)

	evs := w.Poll(context.Background())
	if len(*errs) > 0 {
		t.Fatal(*errs)
	}
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1: %#v", len(evs), evs)
	}
	f, ok := evs[0].(Failed)
	if !ok {
		t.Fatalf("got %T, want Failed", evs[0])
	}
	if f.Task != "t4" || f.Branch != "saddle/t4-ci" || f.Check.Label() != "ci / test" {
		t.Errorf("wrong origin or check: %+v", f)
	}
	if f.RunURL != runURL || f.Step != "Run go test" {
		t.Errorf("RunURL=%q Step=%q", f.RunURL, f.Step)
	}
	want := "starting\n--- FAIL: TestX\n##[error]Process completed with exit code 1."
	if f.LogTail != want {
		t.Errorf("LogTail=%q, want %q", f.LogTail, want)
	}
	msg := f.Message()
	for _, s := range []string{`"ci / test"`, runURL, "--- FAIL: TestX", "saddle/t4-ci"} {
		if !strings.Contains(msg, s) {
			t.Errorf("message missing %q:\n%s", s, msg)
		}
	}
}

func TestRepeatedFailureIsQuiet(t *testing.T) {
	gh := newFake()
	gh.checks("60", check("fail", jobLink))
	w, _ := newWatcher(t, gh, t4)
	ctx := context.Background()

	if n := len(w.Poll(ctx)); n != 1 {
		t.Fatalf("first poll: %d events, want 1", n)
	}
	for i := range 3 {
		if evs := w.Poll(ctx); len(evs) != 0 {
			t.Fatalf("poll %d: got %#v, want nothing", i+2, evs)
		}
	}
	// One checks call per poll, one log fetch in total.
	if n := gh.count("pr checks"); n != 4 {
		t.Errorf("pr checks calls = %d, want 4", n)
	}
	if n := gh.count("run view"); n != 1 {
		t.Errorf("log fetches = %d, want 1", n)
	}

	// Pending in between does not reset the failure.
	gh.checks("60", check("pending", jobLink))
	if evs := w.Poll(ctx); len(evs) != 0 {
		t.Fatalf("pending: %#v", evs)
	}
	gh.checks("60", check("fail", jobLink))
	if evs := w.Poll(ctx); len(evs) != 0 {
		t.Fatalf("same run after pending: %#v", evs)
	}

	// A new run failing again is a new failure.
	gh.checks("60", check("fail", jobLink2))
	evs := w.Poll(ctx)
	if len(evs) != 1 {
		t.Fatalf("new run: %d events, want 1", len(evs))
	}
	if f := evs[0].(Failed); f.RunURL != "https://github.com/o/r/actions/runs/101" {
		t.Errorf("RunURL=%q", f.RunURL)
	}
}

func TestRecovery(t *testing.T) {
	gh := newFake()
	gh.checks("60", check("fail", jobLink))
	w, _ := newWatcher(t, gh, t4)
	ctx := context.Background()
	w.Poll(ctx)

	gh.checks("60", check("pending", jobLink2))
	if evs := w.Poll(ctx); len(evs) != 0 {
		t.Fatalf("pending: %#v", evs)
	}
	gh.checks("60", check("pass", jobLink2))
	evs := w.Poll(ctx)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	r, ok := evs[0].(Recovered)
	if !ok || r.Check.Label() != "ci / test" || r.Task != "t4" {
		t.Fatalf("got %#v", evs[0])
	}
	if !strings.Contains(r.Message(), "passing again") {
		t.Error(r.Message())
	}
	if evs := w.Poll(ctx); len(evs) != 0 {
		t.Fatalf("still passing: %#v", evs)
	}
}

func TestPassingFromStartIsQuiet(t *testing.T) {
	gh := newFake()
	gh.checks("60", check("pass", jobLink))
	w, _ := newWatcher(t, gh, t4)
	if evs := w.Poll(context.Background()); len(evs) != 0 {
		t.Fatalf("got %#v", evs)
	}
}

func TestGHErrors(t *testing.T) {
	gh := newFake()
	gh.checks("60", check("fail", jobLink))
	w, errs := newWatcher(t, gh, t4)
	ctx := context.Background()
	w.Poll(ctx)

	// A failing gh call is reported and leaves the state alone: no
	// recovery, and no duplicate failure once gh works again.
	key := "pr checks 60 --json name,workflow,bucket,link"
	gh.out[key], gh.errs[key] = "", errors.New("gh pr checks: exit status 1: HTTP 502")
	if evs := w.Poll(ctx); len(evs) != 0 {
		t.Fatalf("gh error: %#v", evs)
	}
	if len(*errs) != 1 || !strings.Contains((*errs)[0].Error(), "502") {
		t.Fatalf("errors = %v", *errs)
	}
	gh.checks("60", check("fail", jobLink))
	delete(gh.errs, key)
	if evs := w.Poll(ctx); len(evs) != 0 {
		t.Fatalf("after gh error: %#v", evs)
	}

	// Garbage output with a zero exit is an error too.
	gh.checks("60", "[not json")
	w.Poll(ctx)
	if len(*errs) != 2 {
		t.Fatalf("errors = %v", *errs)
	}
}

func TestNonZeroExitWithJSON(t *testing.T) {
	// gh exits 8 for pending checks but still prints them.
	gh := newFake()
	gh.checks("60", check("fail", jobLink))
	gh.errs["pr checks 60 --json name,workflow,bucket,link"] = errors.New("exit status 8")
	w, errs := newWatcher(t, gh, t4)
	if evs := w.Poll(context.Background()); len(evs) != 1 || len(*errs) != 0 {
		t.Fatalf("events=%#v errors=%v", evs, *errs)
	}
}

func TestNoChecksReported(t *testing.T) {
	gh := newFake()
	gh.errs["pr checks saddle/t9 --json name,workflow,bucket,link"] = errors.New("gh pr checks: exit status 1: no checks reported on the 'saddle/t9' branch")
	w, errs := newWatcher(t, gh, Target{Task: "t9", Branch: "saddle/t9"})
	if evs := w.Poll(context.Background()); len(evs) != 0 || len(*errs) != 0 {
		t.Fatalf("events=%#v errors=%v", evs, *errs)
	}
}

func TestLogFetchError(t *testing.T) {
	gh := newFake()
	gh.checks("60", check("fail", jobLink))
	gh.errs["run view --job 200 --log-failed"] = errors.New("log not found")
	w, _ := newWatcher(t, gh, t4)
	evs := w.Poll(context.Background())
	if len(evs) != 1 {
		t.Fatalf("got %d events", len(evs))
	}
	f := evs[0].(Failed)
	if f.LogErr == "" || f.LogTail != "" || !strings.Contains(f.Message(), "could not be fetched") {
		t.Fatalf("%+v", f)
	}
}

func TestNonActionsCheckSkipsLog(t *testing.T) {
	gh := newFake()
	gh.checks("60", `[{"name":"GitGuardian","bucket":"fail","link":"https://dashboard.gitguardian.com"}]`)
	w, _ := newWatcher(t, gh, t4)
	evs := w.Poll(context.Background())
	if len(evs) != 1 || evs[0].(Failed).RunURL != "https://dashboard.gitguardian.com" {
		t.Fatalf("%#v", evs)
	}
	if n := gh.count("run view"); n != 0 {
		t.Errorf("log fetches = %d, want 0", n)
	}
}

func TestTargetsErrorAndPruning(t *testing.T) {
	gh := newFake()
	gh.checks("60", check("fail", jobLink))
	targets := []Target{t4}
	var tErr error
	var errs []error
	w, err := New(Config{
		GH:      gh.run,
		Targets: func(context.Context) ([]Target, error) { return targets, tErr },
		OnError: func(_ Target, err error) { errs = append(errs, err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	w.Poll(ctx)

	tErr = errors.New("store closed")
	if evs := w.Poll(ctx); len(evs) != 0 || len(errs) != 1 {
		t.Fatalf("events=%#v errors=%v", evs, errs)
	}
	if _, ok := w.Snapshot()["60"]; !ok {
		t.Fatal("state dropped on a Targets error")
	}

	// A PR that is no longer listed is forgotten.
	tErr, targets = nil, nil
	w.Poll(ctx)
	if len(w.Snapshot()) != 0 {
		t.Fatalf("state not pruned: %v", w.Snapshot())
	}
}

func TestSnapshotRestore(t *testing.T) {
	gh := newFake()
	gh.checks("60", check("fail", jobLink))
	w, _ := newWatcher(t, gh, t4)
	w.Poll(context.Background())

	w2, _ := newWatcher(t, gh, t4)
	w2.Restore(w.Snapshot())
	if evs := w2.Poll(context.Background()); len(evs) != 0 {
		t.Fatalf("restored watcher re-reported: %#v", evs)
	}
}

func TestRunPollsUntilCancelled(t *testing.T) {
	gh := newFake()
	gh.checks("60", check("fail", jobLink))
	w, err := New(Config{
		GH:       gh.run,
		Targets:  func(context.Context) ([]Target, error) { return []Target{t4}, nil },
		Interval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var got []Event
	done := make(chan struct{})
	go func() {
		w.Run(ctx, func(e Event) { got = append(got, e) })
		close(done)
	}()
	deadline := time.After(5 * time.Second)
	for {
		w.mu.Lock()
		n := len(gh.calls)
		w.mu.Unlock()
		if n >= 4 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("Run did not keep polling")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
	if len(got) != 1 {
		t.Fatalf("got %d events over several polls, want 1", len(got))
	}
}

func TestNewRequiresConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("want error")
	}
}

func TestParseJobLink(t *testing.T) {
	run, job, ok := parseJobLink("https://github.com/o/r/actions/runs/100/attempts/2/job/200")
	if !ok || run != runURL || job != "200" {
		t.Fatalf("%q %q %v", run, job, ok)
	}
	if _, _, ok := parseJobLink("https://example.com/x"); ok {
		t.Fatal("matched a non-Actions link")
	}
}

func TestReportOmitsFixInstruction(t *testing.T) {
	f := Failed{Origin: Origin{Target: t4}, Check: Check{Name: "test", Workflow: "ci"}, RunURL: runURL, LogTail: "--- FAIL: TestX"}
	r := f.Report()
	if !strings.Contains(r, "--- FAIL: TestX") || !strings.Contains(r, runURL) || strings.Contains(r, "done tool") {
		t.Errorf("report:\n%s", r)
	}
	if !strings.HasPrefix(f.Message(), r) || !strings.Contains(f.Message(), "done tool") {
		t.Errorf("message should be the report plus the fix line:\n%s", f.Message())
	}
}
