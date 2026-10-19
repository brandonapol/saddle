package automerge

import (
	"errors"
	"strings"
	"testing"
)

// batchGH counts every forge read. OpenPRs is one read of every open PR;
// PR is one read of one. A tick should pay for one, however many PRs are
// open (#711).
type batchGH struct {
	*fakeGH
	calls int
	err   error
}

func (b *batchGH) PR(url string) (PR, error) {
	b.calls++
	return b.fakeGH.PR(url)
}

func (b *batchGH) OpenPRs() ([]PR, error) {
	b.calls++
	if b.err != nil {
		return nil, b.err
	}
	var out []PR
	for _, p := range b.prs {
		if p.State == "" || p.State == "OPEN" {
			cp := *p
			out = append(out, cp)
		}
	}
	return out, nil
}

// TestOpenPRsCostOneCallPerTick: N open PRs cost one forge call per tick,
// not one per PR (#711).
func TestOpenPRsCostOneCallPerTick(t *testing.T) {
	const n = 4
	r := newRig(t, "", "a", "b", "c", "d")
	b := &batchGH{fakeGH: r.gh}
	r.w.GH = b
	for range 3 {
		st, err := r.w.Check()
		if err != nil {
			t.Fatal(err)
		}
		if len(st.Stacks) != 1 || len(st.Stacks[0].Nodes) != n || !st.Stacks[0].Ready {
			t.Fatalf("stacks = %+v, want one ready stack of %d", st.Stacks, n)
		}
	}
	if b.calls != 3 {
		t.Fatalf("forge calls = %d, want 3 (one per tick for %d PRs)", b.calls, n)
	}
}

// A quiet tick makes no call. A tick that does read still costs one call
// for every open PR.
func TestOpenPRsBacksOffQuietTicks(t *testing.T) {
	r, clock, _ := pollRig(t, "a", "b")
	b := &batchGH{fakeGH: r.gh}
	r.w.GH = b
	for range 3 {
		if _, err := r.w.Check(); err != nil {
			t.Fatal(err)
		}
		*clock = clock.Add(DefaultInterval)
	}
	if b.calls != 3 {
		t.Fatalf("fresh ticks cost %d calls, want 3 (one each, not one per PR)", b.calls)
	}
	*clock = clock.Add(2 * DefaultQuietAfter)
	if _, err := r.w.Check(); err != nil {
		t.Fatal(err)
	}
	before := b.calls
	var skipped int
	for range 4 {
		*clock = clock.Add(DefaultInterval)
		if _, err := r.w.Check(); err != nil {
			t.Fatal(err)
		}
		if b.calls == before {
			skipped++
		} else if b.calls != before+1 {
			t.Fatalf("a due tick cost %d calls, want 1", b.calls-before)
		}
		before = b.calls
	}
	if skipped == 0 {
		t.Fatal("quiet PRs still cost a forge call every tick")
	}
}

// A PR the open list doesn't have is done, and later ticks don't read it
// on its own.
func TestOpenPRsDropsPRsThatAreNotOpen(t *testing.T) {
	r := newRig(t, "", "a", "b")
	b := &batchGH{fakeGH: r.gh}
	r.w.GH = b
	r.gh.prs["pr/a"].State = "MERGED"
	st, err := r.w.Check()
	if err != nil {
		t.Fatal(err)
	}
	if b.calls != 1 || len(st.Stacks) != 1 || len(st.Stacks[0].Nodes) != 1 || st.Stacks[0].Nodes[0].Task != "b" {
		t.Fatalf("calls %d, stacks %+v, want one call and only b", b.calls, st.Stacks)
	}
	st, err = r.w.Check()
	if err != nil {
		t.Fatal(err)
	}
	if b.calls != 2 || len(st.Stacks) != 1 || st.Stacks[0].Nodes[0].Task != "b" {
		t.Fatalf("second tick calls %d, stacks %+v, want one more call and only b", b.calls, st.Stacks)
	}
}

func TestOpenPRsErrorStaysOnTheNode(t *testing.T) {
	r := newRig(t, "", "a", "b")
	b := &batchGH{fakeGH: r.gh, err: errors.New("rate limited")}
	r.w.GH = b
	st, err := r.w.Check()
	if err != nil {
		t.Fatal(err)
	}
	if b.calls != 1 {
		t.Fatalf("calls = %d, want 1", b.calls)
	}
	var nodes int
	for _, s := range st.Stacks {
		for _, n := range s.Nodes {
			nodes++
			if !strings.Contains(n.Error, "rate limited") {
				t.Fatalf("node = %+v, want the forge error", n)
			}
		}
	}
	if nodes != 2 {
		t.Fatalf("stacks = %+v, want both PRs carrying the error", st.Stacks)
	}
}

// A check that flips on a later tick is what the one read returns, so a PR
// that goes green merges.
func TestOpenPRsSeesACheckChange(t *testing.T) {
	r, clock, _ := pollRig(t, "a")
	b := &batchGH{fakeGH: r.gh}
	r.w.GH = b
	r.w.Default = true
	if _, err := r.w.Check(); err != nil || len(r.gh.merged) != 0 {
		t.Fatal(err)
	}
	r.gh.prs["pr/a"].Checks = ChecksPass
	*clock = clock.Add(DefaultInterval)
	if _, err := r.w.Check(); err != nil {
		t.Fatal(err)
	}
	if len(r.gh.merged) != 1 {
		t.Fatalf("merged = %v, want the PR once its checks pass", r.gh.merged)
	}
}
