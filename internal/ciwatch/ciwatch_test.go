package ciwatch

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
)

const (
	jobLink  = "https://github.com/o/r/actions/runs/100/job/200"
	jobLink2 = "https://github.com/o/r/actions/runs/101/job/201"
	runURL   = "https://github.com/o/r/actions/runs/100"
)

// fakeCI answers Checks by ref and FailedLog by link, and records every call
// as "checks REF" or "log LINK".
type fakeCI struct {
	out     map[string][]forge.CICheck
	errs    map[string]error
	logs    map[string]forge.JobLog
	logErrs map[string]error
	calls   []string
}

func newFake() *fakeCI {
	return &fakeCI{out: map[string][]forge.CICheck{}, errs: map[string]error{},
		logs: map[string]forge.JobLog{}, logErrs: map[string]error{}}
}

func (f *fakeCI) Checks(_ context.Context, ref string) ([]forge.CICheck, error) {
	f.calls = append(f.calls, "checks "+ref)
	return f.out[ref], f.errs[ref]
}

// FailedLog returns the log set for link; any other job link is its run with
// no log, and anything else comes back as it is.
func (f *fakeCI) FailedLog(_ context.Context, link string, _ int) (forge.JobLog, error) {
	f.calls = append(f.calls, "log "+link)
	if l, ok := f.logs[link]; ok {
		return l, f.logErrs[link]
	}
	run, _, _ := strings.Cut(link, "/job/")
	return forge.JobLog{Run: run}, f.logErrs[link]
}

func (f *fakeCI) checks(ref string, cs []forge.CICheck) { f.out[ref] = cs }

func (f *fakeCI) count(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func check(bucket, link string) []forge.CICheck {
	return []forge.CICheck{{Name: "test", Workflow: "ci", Bucket: bucket, Link: link},
		{Name: "GitGuardian", Bucket: "pass", Link: "https://dashboard.gitguardian.com"}}
}

func newWatcher(t *testing.T, gh *fakeCI, targets ...Target) (*Watcher, *[]error) {
	t.Helper()
	var errs []error
	w, err := New(Config{
		CI:      gh,
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
	gh.logs[jobLink] = forge.JobLog{Run: runURL, Step: "Run go test",
		Tail: "starting\n--- FAIL: TestX\n##[error]Process completed with exit code 1."}
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
	if n := gh.count("checks"); n != 4 {
		t.Errorf("checks calls = %d, want 4", n)
	}
	if n := gh.count("log"); n != 1 {
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

	// A failing forge call is reported and leaves the state alone: no
	// recovery, and no duplicate failure once the forge works again.
	key := "60"
	gh.out[key], gh.errs[key] = nil, errors.New("gh pr checks: exit status 1: HTTP 502")
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

}

// TestUnsupportedForgeIsQuiet: a forge without CI ([forge] cli = "none")
// watches nothing and reports no errors.
func TestUnsupportedForgeIsQuiet(t *testing.T) {
	var errs []error
	w, err := New(Config{
		CI:      forge.Unsupported{Reason: "local-only"},
		Targets: func(context.Context) ([]Target, error) { return []Target{t4}, nil },
		OnError: func(_ Target, err error) { errs = append(errs, err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if evs := w.Poll(context.Background()); len(evs) != 0 || len(errs) != 0 {
		t.Fatalf("events=%#v errors=%v", evs, errs)
	}
}

func TestLogFetchError(t *testing.T) {
	gh := newFake()
	gh.checks("60", check("fail", jobLink))
	gh.logErrs[jobLink] = errors.New("log not found")
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

func TestNonActionsCheckHasNoLog(t *testing.T) {
	gh := newFake()
	gh.checks("60", []forge.CICheck{{Name: "GitGuardian", Bucket: "fail", Link: "https://dashboard.gitguardian.com"}})
	w, _ := newWatcher(t, gh, t4)
	evs := w.Poll(context.Background())
	if len(evs) != 1 || evs[0].(Failed).RunURL != "https://dashboard.gitguardian.com" || evs[0].(Failed).LogErr != "" {
		t.Fatalf("%#v", evs)
	}
}

func TestTargetsErrorAndPruning(t *testing.T) {
	gh := newFake()
	gh.checks("60", check("fail", jobLink))
	targets := []Target{t4}
	var tErr error
	var errs []error
	w, err := New(Config{
		CI:      gh,
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
		CI:       gh,
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
