package automerge

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
)

// stepClock is a settable Now for the watcher.
type stepClock struct{ t time.Time }

func (c *stepClock) now() time.Time { return c.t }

func (c *stepClock) add(d time.Duration) { c.t = c.t.Add(d) }

func withClock(r *rig) *stepClock {
	c := &stepClock{t: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	r.w.Now = c.now
	return c
}

func actions(ns []notice) int {
	n := 0
	for _, x := range ns {
		if x.action {
			n++
		}
	}
	return n
}

// PR #811's case (#838): a merge that fails never stops the watcher. A later
// tick, after the backoff, merges it with no `automerge on`.
func TestMergeFailureRetriesWithoutLatch(t *testing.T) {
	r := newRig(t, "", "t1")
	c := withClock(r)
	r.on(t)
	r.gh.mergeErr = errors.New("must be merged using the asynchronous merge REST API")
	st := check(t, r.w)
	if st.Stacks[0].Ready || !strings.Contains(st.Stacks[0].Why, "retrying") {
		t.Fatalf("stack = %+v, want it backing off with the reason", st.Stacks[0])
	}
	// Inside the backoff nothing is tried again.
	r.gh.mergeErr = nil
	check(t, r.w)
	if len(r.gh.merged) != 0 {
		t.Fatalf("retried inside the backoff: %v", r.gh.merged)
	}
	c.add(2 * time.Minute)
	if st := check(t, r.w); st.Merged != "pr/t1" || len(r.gh.merged) != 1 {
		t.Fatalf("didn't retry after the backoff: %+v merged %v", st, r.gh.merged)
	}
	if actions(r.notices) != 0 {
		t.Fatalf("one failure escalated: %+v", r.notices)
	}
}

// A rate limit, however often it comes back, is waited out and never told
// to a human.
func TestTransientMergeFailureNeverEscalates(t *testing.T) {
	r := newRig(t, "", "t1")
	c := withClock(r)
	r.on(t)
	r.gh.mergeErr = fmt.Errorf("gh pr merge: %w", forge.ErrRateLimited)
	for range 50 {
		check(t, r.w)
		c.add(time.Hour)
	}
	if actions(r.notices) != 0 {
		t.Fatalf("a transient failure escalated: %+v", r.notices)
	}
	r.gh.mergeErr = nil
	if st := check(t, r.w); st.Merged != "pr/t1" {
		t.Fatalf("didn't merge once the limit cleared: %+v", st)
	}
}

// A deterministic failure tells the orchestrator once, after three tries,
// and keeps trying at the slow cadence.
func TestDeterministicMergeFailureEscalatesOnce(t *testing.T) {
	r := newRig(t, "", "t1")
	c := withClock(r)
	r.on(t)
	r.gh.mergeErr = errors.New("required review missing")
	tries := 0
	for range 10 {
		before := len(r.events)
		check(t, r.w)
		for _, e := range r.events[before:] {
			if e.kind == EventFailed {
				tries++
			}
		}
		c.add(3 * time.Hour)
	}
	if tries != 10 {
		t.Fatalf("tried %d times in 10 slow ticks, want 10", tries)
	}
	if n := actions(r.notices); n != 1 {
		t.Fatalf("escalated %d times, want once: %+v", n, r.notices)
	}
	if !strings.Contains(r.notices[0].text, "required review missing") || strings.Contains(r.notices[0].text, "automerge on") {
		t.Fatalf("notice = %q", r.notices[0].text)
	}
}

// A new head is a new attempt: the backoff from the old head doesn't apply.
func TestHeadChangeResetsAttempt(t *testing.T) {
	r := newRig(t, "", "t1")
	withClock(r)
	r.on(t)
	r.gh.mergeErr = errors.New("head branch was modified")
	check(t, r.w)
	r.gh.mergeErr = nil
	r.gh.prs["pr/t1"].HeadSHA = "sha-t1-fixed"
	if st := check(t, r.w); st.Merged != "pr/t1" {
		t.Fatalf("new head still backing off: %+v", st.Stacks)
	}
}

// pendingGH is a forge whose merges GitHub accepts and finishes later, as
// merge-async does.
type pendingGH struct {
	*fakeGH
	accepted []string
}

func (p *pendingGH) Merge(url, method, head string) error {
	if p.mergeErr != nil {
		return p.mergeErr
	}
	p.accepted = append(p.accepted, url)
	return fmt.Errorf("#1: %w", forge.ErrMergePending)
}

// finish is GitHub completing the accepted merges.
func (p *pendingGH) finish() {
	for _, u := range p.accepted {
		p.prs[u].State = "MERGED"
		p.merged = append(p.merged, u)
	}
	p.accepted = nil
}

// An async merge that is still pending is in progress: the watcher waits,
// without failing or merging anything else, and restacks once GitHub shows
// the PR merged.
func TestPendingMergeWaitsThenRestacks(t *testing.T) {
	r := newRig(t, "", "t1", "t2")
	withClock(r)
	gh := &pendingGH{fakeGH: r.gh}
	r.w.GH = gh
	r.restack = r.retarget
	r.on(t)
	check(t, r.w)
	if len(gh.accepted) != 1 || r.restacks != 0 {
		t.Fatalf("accepted %v restacks %d", gh.accepted, r.restacks)
	}
	for _, e := range r.events {
		if e.kind == EventFailed {
			t.Fatalf("pending logged as a failure: %+v", e)
		}
	}
	// Still pending: no second merge call, no restack.
	check(t, r.w)
	if len(gh.accepted) != 1 || r.restacks != 0 {
		t.Fatalf("while pending: accepted %v restacks %d", gh.accepted, r.restacks)
	}
	gh.finish()
	check(t, r.w)
	if r.restacks != 1 || r.gh.prs["pr/t2"].Base != "main" {
		t.Fatalf("after the merge: restacks %d, t2 base %s", r.restacks, r.gh.prs["pr/t2"].Base)
	}
}

// (c) of #838: a stack merges all the way down by itself. After each lower
// PR shows merged, restack retargets the next onto main and it merges.
func TestStackMergesDownWithAsyncMerges(t *testing.T) {
	r := newRig(t, "", "t1", "t2", "t3")
	withClock(r)
	gh := &pendingGH{fakeGH: r.gh}
	r.w.GH = gh
	r.restack = r.retarget
	r.on(t)
	for range 12 {
		check(t, r.w)
		gh.finish()
	}
	if len(r.gh.merged) != 3 {
		t.Fatalf("merged %v, want the whole stack", r.gh.merged)
	}
	if actions(r.notices) != 0 {
		t.Fatalf("needed a human: %+v", r.notices)
	}
}

// A restack that fails after a merge is retried on later ticks, not latched.
func TestRestackFailureRetries(t *testing.T) {
	r := newRig(t, "", "t1", "t2")
	c := withClock(r)
	r.on(t)
	r.restack = func() error { return errors.New("stale info") }
	st := check(t, r.w)
	if !st.Enabled || len(r.gh.merged) != 1 {
		t.Fatalf("enabled %v merged %v", st.Enabled, r.gh.merged)
	}
	r.restack = r.retarget
	c.add(time.Hour)
	check(t, r.w) // finishes the restack
	c.add(time.Hour)
	check(t, r.w)
	if len(r.gh.merged) != 2 {
		t.Fatalf("didn't continue after the restack recovered: %v", r.gh.merged)
	}
}

// A stop left in an old state file is cleared, not obeyed.
func TestLegacyStopIsCleared(t *testing.T) {
	r := newRig(t, "", "t1")
	r.on(t)
	b, err := os.ReadFile(r.w.Path)
	if err != nil {
		t.Fatal(err)
	}
	b = bytes.Replace(b, []byte("{"), []byte(`{"stopped": "merging pr/x failed", "attempts": {"merge:pr/t1": {"n": 9}},`), 1)
	if err := os.WriteFile(r.w.Path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	st := check(t, r.w)
	if st.Merged != "pr/t1" {
		t.Fatalf("legacy stop still latches: %+v", st)
	}
}

// saddle/base red on a PR above the bottom says only that the PR below
// hasn't merged; that PR's own CI is still green (#838).
func TestSaddleBaseDoesNotMakeAPRRed(t *testing.T) {
	pr := forgePR(forge.PR{URL: "u", State: "OPEN", Checks: []forge.CheckRun{
		{Kind: forge.KindStatus, Context: forge.BaseStatus, State: "FAILURE"},
		{Kind: forge.KindJob, Name: "test", Status: "COMPLETED", Conclusion: "SUCCESS"},
	}})
	if pr.Checks != ChecksPass {
		t.Fatalf("checks = %s, want pass", pr.Checks)
	}
}

// A restack blocked by a repair's push to the branch waits; it never tells
// a human.
func TestRemoteMovedRestackIsTransient(t *testing.T) {
	if !transient(errors.New("push abc to saddle/t2: remote branch moved to commits the restack doesn't have (origin has def)")) {
		t.Fatal("a moved remote branch isn't transient")
	}
	if transient(errors.New("required review missing")) {
		t.Fatal("a review failure is transient")
	}
}

// asyncGH is a forge whose plain merge GitHub refuses for a PR in a native
// stack; only its async merge path merges.
type asyncGH struct {
	*fakeGH
	async []string
}

func (a *asyncGH) Merge(url, method, head string) error {
	return &forge.RefusedError{Reason: forge.StackedNeedsAsyncMerge,
		Err: errors.New("This pull request is part of a stack and must be merged using the asynchronous merge REST API")}
}

func (a *asyncGH) MergeAsync(url, method, head string) error {
	a.async = append(a.async, url)
	return a.fakeGH.Merge(url, method, head)
}

// #826, PR #811's case: GH.Merge returns the async-merge refusal. Auto-merge
// doesn't stop or escalate, and the next tick merges through the async path.
func TestAsyncMergeRefusalRoutesToAsyncPath(t *testing.T) {
	r := newRig(t, "", "t1")
	withClock(r)
	gh := &asyncGH{fakeGH: r.gh}
	r.w.GH = gh
	r.on(t)
	st := check(t, r.w)
	if !st.Enabled || st.Merged != "" || len(gh.async) != 0 {
		t.Fatalf("first tick = %+v async %v", st, gh.async)
	}
	st = check(t, r.w) // no clock change: a routed refusal is due at once
	if st.Merged != "pr/t1" || len(gh.async) != 1 {
		t.Fatalf("next tick didn't use the async path: %+v async %v", st, gh.async)
	}
	if actions(r.notices) != 0 {
		t.Fatalf("a routed refusal escalated: %+v", r.notices)
	}
}

// The escalation is also an event the blocker sweep files an issue for.
func TestEscalationWritesEvent(t *testing.T) {
	r := newRig(t, "", "t1")
	c := withClock(r)
	r.on(t)
	r.gh.mergeErr = errors.New("required review missing")
	for range 5 {
		check(t, r.w)
		c.add(3 * time.Hour)
	}
	n := 0
	for _, e := range r.events {
		if e.kind == EventEscalated {
			n++
			if e.task != "t1" || !strings.Contains(e.data, "required review missing") {
				t.Fatalf("escalation event = %+v", e)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d escalation events, want 1", n)
	}
}
