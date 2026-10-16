package app

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// digestTarget is an orchestrator input box for FlushDigest.
type digestTarget struct{ busy, drafting atomic.Bool }

func (d *digestTarget) Busy() bool          { return d.busy.Load() }
func (d *digestTarget) Drafting() bool      { return d.drafting.Load() }
func (d *digestTarget) Send(s string) error { return nil }

func feedDigest(t *testing.T, a *App) {
	t.Helper()
	for _, text := range []string{
		`Auto-merged https://github.com/o/r/pull/12 (t1) into main and restacked the rest of stack t1.`,
		`Auto-merged https://github.com/o/r/pull/13 (t2) into main and restacked the rest of stack t2.`,
		`Auto-merged https://github.com/o/r/pull/14 (t3) into main and restacked the rest of stack t3.`,
		`t1 "alpha" landed on saddle/integration at 0123456789ab.`,
		`t2 "beta" landed on saddle/integration at 0123456789ac.`,
		`CI is passing again on t5's PR #15: check "ci" passed.`,
	} {
		if a.admitNotice(OrchestratorID, store.NoticeInfo, text) {
			t.Fatalf("%q interrupted", text)
		}
	}
}

// #222: routine notices in one window become a single compact digest line,
// sent once the window has passed.
func TestDigestCoalescesWindow(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.Cfg.Notices.DigestEvery = 15 * time.Minute
	feedDigest(t, a)
	now := time.Now()
	if sent, err := a.FlushDigest(now); err != nil || sent != "" {
		t.Fatalf("digest sent before its window passed: %q, %v", sent, err)
	}
	sent, err := a.FlushDigest(now.Add(16 * time.Minute))
	must(t, err)
	for _, want := range []string{"Since ", "3 PRs merged (#12 #13 #14)", "2 tasks landed (t1 t2)", "CI green on 1"} {
		if !strings.Contains(sent, want) {
			t.Fatalf("digest %q lacks %q", sent, want)
		}
	}
	if strings.Contains(sent, "\n") {
		t.Fatalf("digest is not one line: %q", sent)
	}
	if strings.Contains(sent, "notices --all") {
		t.Fatalf("digest repeats the notices hint (#402): %q", sent)
	}
	ns := pendingOrch(t, a)
	if len(ns) != 1 || ns[0].Kind != store.NoticeInfo || ns[0].Text != sent {
		t.Fatalf("orchestrator queue = %+v, want the one digest as info", ns)
	}
	if again, err := a.FlushDigest(now.Add(40 * time.Minute)); err != nil || again != "" {
		t.Fatalf("digest sent twice: %q, %v", again, err)
	}
	if got := noticeEvents(t, a, EventDigestSent); len(got) != 1 {
		t.Fatalf("digest_sent events = %q", got)
	}
}

// #222: the digest waits while the orchestrator is busy or the owner types,
// and goes out once it is idle.
func TestDigestOnlyWhenIdle(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	tgt := &digestTarget{}
	a.SetCompactTarget(tgt)
	t.Cleanup(func() { a.SetCompactTarget(nil) })
	feedDigest(t, a)
	later := time.Now().Add(time.Hour)

	tgt.busy.Store(true)
	if sent, _ := a.FlushDigest(later); sent != "" {
		t.Fatalf("digest sent while the orchestrator was busy: %q", sent)
	}
	tgt.busy.Store(false)
	tgt.drafting.Store(true)
	if sent, _ := a.FlushDigest(later); sent != "" {
		t.Fatalf("digest sent while the owner was typing: %q", sent)
	}
	if ns := pendingOrch(t, a); len(ns) != 0 {
		t.Fatalf("queued while not idle: %+v", ns)
	}
	tgt.drafting.Store(false)
	if sent, _ := a.FlushDigest(later); sent == "" {
		t.Fatal("digest not sent once idle")
	}
}

// #222: nothing worth telling, no digest: silenced notices alone send none.
func TestDigestSkipsWhenNothingHappened(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	if a.admitNotice(OrchestratorID, store.NoticeInfo, `Restacked 2 landed tasks onto origin/main at abc: 0 refs moved, 0 commits already in base dropped.`) {
		t.Fatal("no-op restack interrupted")
	}
	if sent, err := a.FlushDigest(time.Now().Add(time.Hour)); err != nil || sent != "" {
		t.Fatalf("digest of nothing: %q, %v", sent, err)
	}
}

// #222: an interrupt is admitted at once, never held for the digest window,
// and routine notices around it don't hold it back either.
func TestInterruptNeverDelayed(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	feedDigest(t, a)
	if !a.admitNotice(OrchestratorID, store.NoticeAction, `t7 asks: should the cache be per-user or global?`) {
		t.Fatal("question held back")
	}
	if got := noticeEvents(t, a, EventDigestSent); len(got) != 0 {
		t.Fatalf("the interrupt flushed the digest early: %q", got)
	}
}

// #222: Digest previews the unsent digest for `saddle notices`.
func TestDigestPreview(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	if line, err := a.PendingDigest(); err != nil || line != "" {
		t.Fatalf("empty preview = %q, %v", line, err)
	}
	feedDigest(t, a)
	line, err := a.PendingDigest()
	must(t, err)
	if !strings.Contains(line, "3 PRs merged") {
		t.Fatalf("preview = %q", line)
	}
}

// #402: consecutive digest lines merge into one rolling line that counts
// everything since the first.
func TestMergeDigestsRolls(t *testing.T) {
	t.Parallel()
	lines := []string{
		"Since 09:23: 3 PRs merged (#12 #13 #14), restacked 3 times.",
		"Since 09:40: 1 PR merged (#15), 1 task landed (t4), restacked once, stack checks clean.",
		"Since 10:02: 3 PRs merged (#16 #17 #18), restacked 3 times, stack checks clean (2x), CI green on 1.",
	}
	got := lines[0]
	for _, l := range lines[1:] {
		var ok bool
		if got, ok = MergeDigests(got, l); !ok {
			t.Fatalf("MergeDigests refused %q", l)
		}
	}
	want := "Since 09:23: 7 PRs merged (#12 #13 #14 #15 #16 #17 #18), 1 task landed (t4), restacked 7 times, CI green on 1, stack checks clean (3x)."
	if got != want {
		t.Fatalf("merged =\n%s\nwant\n%s", got, want)
	}
	if _, ok := MergeDigests(got, "t3 landed on main"); ok {
		t.Fatal("merged a non-digest line")
	}
	if IsDigest("Since we spoke: nothing.") {
		t.Fatal("prose taken for a digest")
	}
}

// #402: every line summarizeDigest writes parses back.
func TestDigestLineRoundTrips(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.Cfg.Notices.DigestEvery = time.Minute
	feedDigest(t, a)
	sent, err := a.FlushDigest(time.Now().Add(time.Hour))
	must(t, err)
	if !IsDigest(sent) {
		t.Fatalf("digest %q doesn't parse back", sent)
	}
	if again, ok := MergeDigests(sent, sent); !ok || !strings.Contains(again, "6 PRs merged") {
		t.Fatalf("self-merge = %q, %v", again, ok)
	}
}

// #681: a digest of merged GitLab MRs says "MRs", not "PRs", and the line
// reads back and folds with a GitHub one.
func TestDigestSaysMRsOnGitLab(t *testing.T) {
	t.Parallel()
	d := newDigest("10:15")
	d.n[TopicMerged] = 2
	d.ids[TopicMerged] = []string{"!12", "!13"}
	got := d.String()
	if want := "Since 10:15: 2 MRs merged (!12 !13)."; got != want {
		t.Fatalf("digest = %q, want %q", got, want)
	}
	d.n[TopicMerged], d.ids[TopicMerged] = 1, []string{"!12"}
	got = d.String()
	if want := "Since 10:15: 1 MR merged (!12)."; got != want {
		t.Fatalf("digest = %q, want %q", got, want)
	}
	back, ok := parseDigest(got)
	if !ok || back.n[TopicMerged] != 1 || len(back.ids[TopicMerged]) != 1 || back.ids[TopicMerged][0] != "!12" {
		t.Fatalf("parseDigest(%q) = %+v, %v", got, back, ok)
	}
	merged, ok := MergeDigests("Since 10:15: 2 MRs merged (!12 !13).", "Since 10:30: 1 MR merged (!14).")
	if !ok || !strings.Contains(merged, "3 MRs merged (!12 !13 !14)") {
		t.Fatalf("MergeDigests = %q, %v", merged, ok)
	}
	// GitHub keeps its wording.
	d.ids[TopicMerged] = []string{"#12"}
	if got := d.String(); !strings.Contains(got, "1 PR merged (#12)") {
		t.Fatalf("github digest = %q", got)
	}
}
