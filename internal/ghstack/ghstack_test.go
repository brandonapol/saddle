package ghstack

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// fake is a gh runner that records calls and answers from a table keyed by
// the call's leading args.
type fake struct {
	calls   [][]string
	answers map[string]answer
}

type answer struct {
	out string
	err error
}

func (f *fake) run(args ...string) (string, error) {
	f.calls = append(f.calls, args)
	line := strings.Join(args, " ")
	best := ""
	for k := range f.answers {
		if strings.HasPrefix(line, k) && len(k) > len(best) {
			best = k
		}
	}
	if best == "" {
		return "", fmt.Errorf("gh %s: unexpected call", line)
	}
	a := f.answers[best]
	return a.out, a.err
}

func client(answers map[string]answer) (Client, *fake) {
	f := &fake{answers: answers}
	return Client{Run: f.run}, f
}

func TestAvailableParsesVersion(t *testing.T) {
	c, _ := client(map[string]answer{"stack --version": {out: "gh stack version 0.1.1"}})
	v, err := c.Available()
	if err != nil || v != "0.1.1" {
		t.Fatalf("Available = %q, %v", v, err)
	}
}

func TestAvailableNotInstalled(t *testing.T) {
	c, _ := client(map[string]answer{"stack --version": {err: errors.New(`gh stack: exit status 1: unknown command "stack" for "gh"`)}})
	_, err := c.Available()
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("err = %v, want ErrNotInstalled", err)
	}
}

func TestEnabledProbesStacksReadOnly(t *testing.T) {
	c, f := client(map[string]answer{"api repos/{owner}/{repo}/stacks": {out: "[]"}})
	on, err := c.Enabled()
	if err != nil || !on {
		t.Fatalf("Enabled = %v, %v", on, err)
	}
	if got := f.calls[0]; got[0] != "api" || slicesHas(got, "-X") || slicesHas(got, "--method") {
		t.Fatalf("probe must be a plain GET: %q", got)
	}
}

func TestEnabledFalseWhenNotEnabledOrUnknown(t *testing.T) {
	for name, e := range map[string]error{
		"not enabled": errors.New("gh api: exit status 1: Stacked PRs are not enabled for this repository (HTTP 404)"),
		"not found":   errors.New("gh api: exit status 1: gh: Not Found (HTTP 404)"),
		"unknown":     errors.New("gh api: exit status 1: dial tcp: no route to host"),
	} {
		c, _ := client(map[string]answer{"api repos/{owner}/{repo}/stacks": {err: e}})
		on, err := c.Enabled()
		if on {
			t.Fatalf("%s: Enabled = true", name)
		}
		if name != "unknown" && !errors.Is(err, ErrNotEnabled) {
			t.Fatalf("%s: err = %v, want ErrNotEnabled", name, err)
		}
		if name == "unknown" && (err == nil || errors.Is(err, ErrNotEnabled)) {
			t.Fatalf("unknown: err = %v, want the probe's error", err)
		}
	}
}

func TestLinkPassesRefsBottomToTopWithBase(t *testing.T) {
	c, f := client(map[string]answer{"stack link": {out: "Created stack #9 with 2 PRs"}})
	if err := c.Link("main", "https://github.com/o/r/pull/4", "https://github.com/o/r/pull/2"); err != nil {
		t.Fatal(err)
	}
	want := []string{"stack", "link", "--base", "main", "https://github.com/o/r/pull/4", "https://github.com/o/r/pull/2"}
	if !reflect.DeepEqual(f.calls[0], want) {
		t.Fatalf("link call = %q, want %q", f.calls[0], want)
	}
}

func TestLinkNeedsTwoRefs(t *testing.T) {
	c, f := client(nil)
	if err := c.Link("main", "4"); err == nil {
		t.Fatal("linking one PR should fail")
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls = %q", f.calls)
	}
}

func TestLinkTypedErrors(t *testing.T) {
	for msg, want := range map[string]error{
		`unknown command "stack" for "gh"`:                   ErrNotInstalled,
		"Stacked PRs are not enabled for this repository":    ErrNotEnabled,
		"#4 belongs to multiple stacks on GitHub (7, 8)":     ErrConflict,
		"a PR in the arguments belongs to a different stack": ErrConflict,
	} {
		c, _ := client(map[string]answer{"stack link": {err: errors.New("gh stack link: exit status 1: " + msg)}})
		err := c.Link("main", "4", "5")
		if !errors.Is(err, want) {
			t.Fatalf("%q: err = %v, want %v", msg, err, want)
		}
		var e *Error
		if !errors.As(err, &e) || e.Op != "link" {
			t.Fatalf("%q: err %v is not a *Error for link", msg, err)
		}
	}
}

const stacksJSON = `[{"number":7,"base":{"ref":"main"},"open":true,"pull_requests":[
 {"number":4,"state":"open","draft":false,"merged_at":null,"head":{"ref":"saddle/t1","sha":"a"}},
 {"number":5,"state":"open","draft":false,"merged_at":null,"head":{"ref":"saddle/t2","sha":"b"}}]},
 {"number":3,"base":{"ref":"main"},"open":false,"pull_requests":[
 {"number":1,"state":"closed","draft":false,"merged_at":"2026-10-01T00:00:00Z","head":{"ref":"x","sha":"c"}}]}]`

func TestViewFindsStackByPRBranchOrNumber(t *testing.T) {
	c, _ := client(map[string]answer{"api repos/{owner}/{repo}/stacks": {out: stacksJSON}})
	for _, ref := range []string{"5", "#5", "https://github.com/o/r/pull/5", "saddle/t2", "7"} {
		s, err := c.View(ref)
		if err != nil {
			t.Fatalf("View(%q): %v", ref, err)
		}
		if s.Number != 7 || s.Base != "main" || !s.Open || len(s.PRs) != 2 || s.PRs[1].Number != 5 || s.PRs[1].Head != "saddle/t2" {
			t.Fatalf("View(%q) = %+v", ref, s)
		}
	}
	if s, err := c.View("1"); err != nil || s.Number != 3 || !s.PRs[0].Merged() {
		t.Fatalf("View(1) = %+v, %v", s, err)
	}
	if _, err := c.View("99"); !errors.Is(err, ErrNoStack) {
		t.Fatalf("View(99) err = %v, want ErrNoStack", err)
	}
}

func TestViewReadsPaginatedPages(t *testing.T) {
	c, _ := client(map[string]answer{"api repos/{owner}/{repo}/stacks": {out: `[{"number":1,"base":{"ref":"main"},"open":true,"pull_requests":[]}]
[{"number":2,"base":{"ref":"main"},"open":true,"pull_requests":[{"number":9,"state":"open","head":{"ref":"b"}}]}]`}})
	s, err := c.View("9")
	if err != nil || s.Number != 2 {
		t.Fatalf("View = %+v, %v", s, err)
	}
}

func TestMergeArgs(t *testing.T) {
	c, f := client(map[string]answer{"stack merge": {out: "Merged stack"}})
	if err := c.Merge("12", "squash"); err != nil {
		t.Fatal(err)
	}
	if err := c.Merge("12", ""); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"stack", "merge", "12", "--yes", "--merge-method", "squash"},
		{"stack", "merge", "12", "--yes"},
	}
	if !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("merge calls = %q, want %q", f.calls, want)
	}
}

func TestMergeTypedErrors(t *testing.T) {
	c, _ := client(map[string]answer{"stack merge": {err: errors.New("gh stack merge: exit status 1: Async stack merge is not available for this repository")}})
	if err := c.Merge("12", ""); !errors.Is(err, ErrNotEnabled) {
		t.Fatalf("err = %v, want ErrNotEnabled", err)
	}
	c, _ = client(map[string]answer{"stack merge": {err: errors.New("gh stack merge: exit status 1: PR #5 is not mergeable: required checks failing")}})
	err := c.Merge("12", "")
	if !errors.Is(err, ErrMergeRefused) {
		t.Fatalf("err = %v, want ErrMergeRefused", err)
	}
	if !strings.Contains(err.Error(), "required checks failing") {
		t.Fatalf("err %q lost gh's reason", err)
	}
}

func slicesHas(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
