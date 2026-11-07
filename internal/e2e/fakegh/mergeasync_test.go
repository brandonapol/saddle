package fakegh

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
)

// GitHub refuses gh pr merge for a PR in a native stack and merges it only
// through PUT pulls/{n}/merge-async, which answers pending; the PR then
// shows MERGED, and GET on the endpoint is a 404 (#838).
func TestMergeAsyncForStackedPRs(t *testing.T) {
	f := newFixture(t)
	a, b := f.openStack()
	f.must("stack", "link", "--base", "main", "1", "2")
	_, errs, code := f.ghRun("pr", "merge", "1", "--squash")
	if code == 0 || !strings.Contains(errs, "asynchronous merge REST API") {
		t.Fatalf("gh pr merge of a stacked PR: exit %d, %q", code, errs)
	}

	head := f.view("1")["headRefOid"].(string)
	_, errs, code = f.ghRun("api", "-X", "PUT", "repos/{owner}/{repo}/pulls/1/merge-async", "-f", "merge_method=squash", "-f", "sha=deadbeef")
	if code == 0 || !strings.Contains(errs, "branch was modified") {
		t.Fatalf("merge-async with a stale sha: exit %d, %q", code, errs)
	}
	out := f.must("api", "-X", "PUT", "repos/{owner}/{repo}/pulls/1/merge-async", "-f", "merge_method=squash", "-f", "sha="+head)
	var resp struct {
		Status  string `json:"status"`
		Details struct {
			UUID string `json:"uuid"`
		} `json:"details"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil || resp.Status != "pending" || resp.Details.UUID == "" {
		t.Fatalf("merge-async = %s (%v)", out, err)
	}
	if st := f.view("1")["state"]; st != "MERGED" {
		t.Fatalf("PR 1 state = %v, want MERGED", st)
	}
	if got := run(t, f.bare, "git", "show", "main:a.txt"); got != "a" {
		t.Fatalf("main lacks a's work: %q", got)
	}
	if _, _, code := f.ghRun("api", "repos/{owner}/{repo}/pulls/1/merge-async"); code == 0 {
		t.Fatal("GET merge-async answered; GitHub 404s")
	}
	s, _ := f.gh.Load()
	if s.PR(a).State != "MERGED" || s.PR(b).State != "OPEN" {
		t.Fatalf("states: %s %s", s.PR(a).State, s.PR(b).State)
	}
	// A PR outside any stack still merges the normal way.
	f.branch("c", "c.txt", "c\n")
	f.must("pr", "create", "--base", "main", "--head", "c", "--title", "C", "--body", "")
	f.must("pr", "merge", "c", "--squash")
}

// saddle's forge merges a stacked PR end to end against the fake: gh pr
// merge is refused, merge-async takes it, and the poll sees it merged.
func TestForgeMergePRThroughMergeAsync(t *testing.T) {
	f := newFixture(t)
	f.openStack()
	f.must("stack", "link", "--base", "main", "1", "2")
	g := &forge.GitHub{
		Run: func(_ context.Context, args ...string) (string, error) {
			out, errs, code := f.ghRun(args...)
			if code != 0 {
				return out, fmt.Errorf("gh %s: exit %d: %s", strings.Join(args[:min(2, len(args))], " "), code, errs)
			}
			return out, nil
		},
		MergePoll: []time.Duration{0},
		Sleep:     func(time.Duration) {},
	}
	head := f.view("1")["headRefOid"].(string)
	if err := g.MergePR(context.Background(), "https://github.com/e2e/demo/pull/1", "squash", head); err != nil {
		t.Fatalf("MergePR = %v", err)
	}
	if st := f.view("1")["state"]; st != "MERGED" {
		t.Fatalf("state = %v", st)
	}
}
