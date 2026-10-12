package doctor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
)

// fakePredict is a quiet machine and repo: every probe finds nothing until
// a test changes a field.
type fakePredict struct {
	cfg        config.Config
	protection string // gh api .../protection output; "" is unprotected
	authStatus string
	authErr    error
	rateLimit  string
	workflows  bool
	autoMerge  bool
	disks      []Disk
	load1      float64
	cpus       int
	mem        uint64
	env        map[string]string
	claudeNow  string
	claudeLast string
	esc        []Escalation
}

func quiet() *fakePredict {
	cfg := config.Default()
	cfg.Concurrency = 2
	return &fakePredict{
		cfg:        cfg,
		authStatus: "Logged in\n  - Token scopes: 'repo', 'workflow'",
		rateLimit:  `{"resources":{"core":{"limit":5000,"remaining":4900,"reset":0}}}`,
		disks:      []Disk{{Label: "the worktrees", Path: "/w", Free: 200 * gb, FreeInodes: 1e7, Inodes: 2e7}},
		cpus:       16,
		load1:      1,
		mem:        32 * gb,
		env:        map[string]string{"DISABLE_AUTOUPDATER": "1"},
		claudeNow:  "2.1.0",
		claudeLast: "2.1.0",
	}
}

func (f *fakePredict) Config() (config.Config, error) { return f.cfg, nil }
func (f *fakePredict) GH(args ...string) (string, error) {
	j := strings.Join(args, " ")
	switch {
	case strings.HasSuffix(j, "/protection"):
		if f.protection == "" {
			return "", errors.New("HTTP 404: Branch not protected")
		}
		return f.protection, nil
	case j == "api rate_limit":
		return f.rateLimit, nil
	}
	return "", errors.New("unexpected gh " + j)
}
func (f *fakePredict) Exec(name string, args ...string) (string, error) {
	if name == "gh" && strings.Join(args, " ") == "auth status" {
		return f.authStatus, f.authErr
	}
	return "", errors.New("not set")
}
func (f *fakePredict) WorkflowFiles() bool                         { return f.workflows }
func (f *fakePredict) AutoMerge() bool                             { return f.autoMerge }
func (f *fakePredict) Disks() []Disk                               { return f.disks }
func (f *fakePredict) Load() (float64, int)                        { return f.load1, f.cpus }
func (f *fakePredict) MemAvailable() uint64                        { return f.mem }
func (f *fakePredict) Getenv(k string) string                      { return f.env[k] }
func (f *fakePredict) ClaudeVersions() (string, string)            { return f.claudeNow, f.claudeLast }
func (f *fakePredict) Escalations(time.Time) ([]Escalation, error) { return f.esc, nil }

var predictNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// findRisk returns the first risk of class, failing the test if there is none.
func findRisk(t *testing.T, rs []Risk, class string) Risk {
	t.Helper()
	for _, r := range rs {
		if r.Class == class {
			return r
		}
	}
	t.Fatalf("no %s risk in %+v", class, rs)
	return Risk{}
}

func TestPredictQuietMachineFindsNothing(t *testing.T) {
	if rs := Predict(quiet(), predictNow); len(rs) != 0 {
		t.Fatalf("want no risks, got %+v", rs)
	}
}

func TestPredictRequiredReviewsWithAutoMergeIsHigh(t *testing.T) {
	f := quiet()
	f.protection = `{"required_pull_request_reviews":{"required_approving_review_count":1},"required_status_checks":{"contexts":["ci"]}}`
	f.autoMerge = true
	r := findRisk(t, Predict(f, predictNow), ClassBranchProtection)
	if r.Severity != High || !strings.Contains(r.What, "auto-merge") || r.Fix == "" {
		t.Fatalf("got %+v", r)
	}
	// Without auto-merge a required review is what the owner wants.
	f.autoMerge = false
	for _, r := range Predict(f, predictNow) {
		if r.Class == ClassBranchProtection {
			t.Fatalf("reviews without auto-merge flagged: %+v", r)
		}
	}
}

func TestPredictMissingWorkflowScopeWithWorkflowsIsHigh(t *testing.T) {
	f := quiet()
	f.authStatus = "Logged in\n  - Token scopes: 'repo', 'read:org'"
	f.workflows = true
	r := findRisk(t, Predict(f, predictNow), ClassCredentials)
	if r.Severity != High || !strings.Contains(r.Fix, "-s workflow") {
		t.Fatalf("got %+v", r)
	}
	f.workflows = false
	if r := findRisk(t, Predict(f, predictNow), ClassCredentials); r.Severity != Low {
		t.Fatalf("no workflow files: want Low, got %+v", r)
	}
}

func TestPredictDiskUnderFiveGBIsHigh(t *testing.T) {
	f := quiet()
	f.disks = []Disk{{Label: "tmp", Path: "/tmp", Free: 4 * gb, FreeInodes: 1e6, Inodes: 2e6}}
	r := findRisk(t, Predict(f, predictNow), ClassDisk)
	if r.Severity != High || !strings.Contains(r.What, "4.0 GB") || !strings.Contains(r.What, "/tmp") {
		t.Fatalf("got %+v", r)
	}
	f.disks[0].Free = 10 * gb
	if r := findRisk(t, Predict(f, predictNow), ClassDisk); r.Severity != Medium {
		t.Fatalf("10 GB: want Medium, got %+v", r)
	}
	f.disks[0].Free, f.disks[0].FreeInodes = 100*gb, 5000
	if r := findRisk(t, Predict(f, predictNow), ClassDisk); r.Severity != High || !strings.Contains(r.What, "inodes") {
		t.Fatalf("few inodes: got %+v", r)
	}
}

func TestPredictHistoryScannerWithSecretGateOffIsHigh(t *testing.T) {
	f := quiet()
	f.protection = `{"required_status_checks":{"checks":[{"context":"GitGuardian Security Checks"},{"context":"codecov/patch"}]}}`
	rs := Predict(f, predictNow)
	if r := findRisk(t, rs, ClassHistoryCheck); r.Severity != Low {
		t.Fatalf("gate on: want Low, got %+v", r)
	}
	if r := findRisk(t, rs, ClassExternalCheck); r.Severity != Medium || !strings.Contains(r.What, "codecov") {
		t.Fatalf("got %+v", r)
	}
	f.cfg.CI.NoSecretGate = true
	if r := findRisk(t, Predict(f, predictNow), ClassHistoryCheck); r.Severity != High || !strings.Contains(r.Fix, "no_secret_gate") {
		t.Fatalf("gate off: got %+v", r)
	}
}

func TestPredictLoadAndClaude(t *testing.T) {
	f := quiet()
	f.load1, f.cpus = 40, 8
	f.claudeLast = "2.0.9"
	delete(f.env, "DISABLE_AUTOUPDATER")
	rs := Predict(f, predictNow)
	if r := findRisk(t, rs, ClassLoad); r.Severity != High {
		t.Fatalf("load 40 on 8: got %+v", r)
	}
	if r := findRisk(t, rs, ClassClaudeCLI); r.Severity != Medium || !strings.Contains(r.What, "2.0.9") {
		t.Fatalf("drift: got %+v", r)
	}
	if rs[0].Severity != High {
		t.Fatalf("not ranked: %+v", rs)
	}
}

func TestPredictLoggedOutIsHigh(t *testing.T) {
	f := quiet()
	f.authErr = errors.New("exit 1")
	if r := findRisk(t, Predict(f, predictNow), ClassCredentials); r.Severity != High || r.Fix != "gh auth login" {
		t.Fatalf("got %+v", r)
	}
}

// The repo's escalations weigh the risks: a class it keeps hitting is
// bumped and counted, one no probe found today still shows, and causes
// that fit nothing are listed as unknown.
func TestRankWeightsByHistory(t *testing.T) {
	f := quiet()
	f.disks[0].Free = 10 * gb // Medium on its own
	f.esc = []Escalation{
		{Kind: app.BlockerTrainEscalated, Cause: "write: no space left on device"},
		{Kind: app.BlockerTrainEscalated, Cause: "disk quota exceeded"},
		{Kind: app.BlockerHistoryCheck, Cause: "GitGuardian Security Checks"},
		{Kind: app.BlockerHistoryCheck, Cause: "GitGuardian Security Checks"},
		{Kind: app.BlockerHistoryCheck, Cause: "GitGuardian Security Checks"},
		{Kind: app.BlockerCIRed, Cause: "lint / golangci"},
	}
	rs := Predict(f, predictNow)
	d := findRisk(t, rs, ClassDisk)
	if d.Severity != High || d.Seen != 2 || !strings.Contains(d.Line(), "this repo: 2 in 14 days") {
		t.Fatalf("disk: %+v %q", d, d.Line())
	}
	if h := findRisk(t, rs, ClassHistoryCheck); h.Severity != Medium || h.Seen != 3 {
		t.Fatalf("history: %+v", h)
	}
	if u := findRisk(t, rs, ClassUnknown); !strings.Contains(u.What, "lint / golangci") || !strings.Contains(u.Fix, "FAILURE-MODES.md") {
		t.Fatalf("unknown: %+v", u)
	}
}

func TestRankCapsAtSeven(t *testing.T) {
	var rs []Risk
	for range 10 {
		rs = append(rs, Risk{Severity: Low, Class: ClassLoad, What: "x", Fix: "y"})
	}
	rs = append(rs, Risk{Severity: High, Class: ClassDisk, What: "x", Fix: "y"})
	got := Rank(rs, nil, nil)
	if len(got) != MaxRisks || got[0].Class != ClassDisk {
		t.Fatalf("got %d, first %+v", len(got), got[0])
	}
}

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		e    Escalation
		want string
	}{
		{Escalation{Kind: app.BlockerStuckStack, Cause: "ci_red"}, ClassStuckStack},
		{Escalation{Kind: app.BlockerCIRed, Cause: "e2e / test: context deadline exceeded"}, ClassLoad},
		{Escalation{Kind: app.BlockerCIRed, Cause: "TruffleHog"}, ClassHistoryCheck},
		{Escalation{Kind: app.BlockerTrainEscalated, Cause: "refusing to allow an OAuth App to create or update workflow"}, ClassCredentials},
		{Escalation{Kind: app.BlockerCIRed, Cause: "build"}, ClassUnknown},
	} {
		if got := Classify(c.e, nil); got != c.want {
			t.Errorf("%+v: got %s, want %s", c.e, got, c.want)
		}
	}
}
