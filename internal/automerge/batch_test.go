package automerge

import "testing"

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
