package automerge

import (
	"testing"
	"time"
)

// pollRig is a rig whose clock the test moves and whose PR reads are counted.
func pollRig(t *testing.T, tasks ...string) (*rig, *time.Time, map[string]int) {
	t.Helper()
	r := newRig(t, "", tasks...)
	on := true
	r.w.Default = on
	clock := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	r.w.Now = func() time.Time { return clock }
	reads := map[string]int{}
	r.gh.onPR = func(url string) { reads[url]++ }
	// Keep PRs unmerged so only polling is under test.
	r.gh.prs["pr/"+tasks[0]].Checks = ChecksPending
	return r, &clock, reads
}

func TestPollBacksOffWhileChecksUnchanged(t *testing.T) {
	r, clock, reads := pollRig(t, "a")
	// Fresh PRs are read every tick.
	for range 3 {
		if _, err := r.w.Check(); err != nil {
			t.Fatal(err)
		}
		*clock = clock.Add(DefaultInterval)
	}
	if reads["pr/a"] != 3 {
		t.Fatalf("fresh PR reads = %d, want 3", reads["pr/a"])
	}
	// Quiet past the threshold: ticks start being skipped.
	*clock = clock.Add(2 * DefaultQuietAfter)
	if _, err := r.w.Check(); err != nil {
		t.Fatal(err)
	}
	before := reads["pr/a"]
	var skipped int
	for range 4 {
		*clock = clock.Add(DefaultInterval)
		if _, err := r.w.Check(); err != nil {
			t.Fatal(err)
		}
		if reads["pr/a"] == before {
			skipped++
		}
		before = reads["pr/a"]
	}
	if skipped == 0 {
		t.Fatalf("unchanged PR was read every tick; want it to back off")
	}
	// The gap grows: far in the future it is still polled, but no later than MaxPollGap.
	*clock = clock.Add(DefaultMaxPollGap)
	n := reads["pr/a"]
	if _, err := r.w.Check(); err != nil {
		t.Fatal(err)
	}
	if reads["pr/a"] != n+1 {
		t.Fatalf("PR not re-read after MaxPollGap")
	}
}

func TestPollGapGrowsThenResetsOnChange(t *testing.T) {
	r, clock, reads := pollRig(t, "a")
	if _, err := r.w.Check(); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(3 * DefaultQuietAfter)
	if _, err := r.w.Check(); err != nil { // read; unchanged for a long while
		t.Fatal(err)
	}
	g1 := r.w.pollGap("pr/a")
	*clock = clock.Add(g1)
	if _, err := r.w.Check(); err != nil {
		t.Fatal(err)
	}
	g2 := r.w.pollGap("pr/a")
	if g2 <= g1 && g1 < DefaultMaxPollGap {
		t.Fatalf("gap did not grow: %v then %v", g1, g2)
	}
	// Checks change: the next due read sees it and the gap resets.
	r.gh.prs["pr/a"].Checks = ChecksFail
	*clock = clock.Add(g2)
	n := reads["pr/a"]
	if _, err := r.w.Check(); err != nil {
		t.Fatal(err)
	}
	if reads["pr/a"] != n+1 {
		t.Fatal("due PR not read")
	}
	if g := r.w.pollGap("pr/a"); g != 0 {
		t.Fatalf("gap after change = %v, want 0 (poll every tick)", g)
	}
	*clock = clock.Add(DefaultInterval)
	n = reads["pr/a"]
	if _, err := r.w.Check(); err != nil {
		t.Fatal(err)
	}
	if reads["pr/a"] != n+1 {
		t.Fatal("changed PR not polled on the next tick")
	}
}

func TestPollStopsForMergedAndClosedPRs(t *testing.T) {
	for _, state := range []string{"MERGED", "CLOSED"} {
		r, clock, reads := pollRig(t, "a", "b")
		r.gh.prs["pr/b"].Checks = ChecksPending
		r.gh.prs["pr/a"].State = state
		for range 3 {
			if _, err := r.w.Check(); err != nil {
				t.Fatal(err)
			}
			*clock = clock.Add(DefaultInterval)
		}
		if reads["pr/a"] != 1 {
			t.Fatalf("%s PR read %d times, want 1", state, reads["pr/a"])
		}
		if reads["pr/b"] != 3 {
			t.Fatalf("open PR read %d times, want 3", reads["pr/b"])
		}
	}
}

func TestPollCachedPRStillMergesWhenReady(t *testing.T) {
	r, clock, _ := pollRig(t, "a")
	r.gh.prs["pr/a"].Checks = ChecksPending
	r.w.Default = true
	if _, err := r.w.Check(); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(3 * DefaultQuietAfter)
	if _, err := r.w.Check(); err != nil {
		t.Fatal(err)
	}
	r.gh.prs["pr/a"].Checks = ChecksPass
	*clock = clock.Add(DefaultMaxPollGap)
	if _, err := r.w.Check(); err != nil {
		t.Fatal(err)
	}
	if len(r.gh.merged) != 1 {
		t.Fatalf("merged = %v, want pr/a merged once green is seen", r.gh.merged)
	}
}
