package automerge

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func high(why string) Result { return Result{Tier: TierHigh, Reasons: []string{why}} }

func TestGateEveryPRAtOrBelowMax(t *testing.T) {
	low := Assessed{PR: "pr/1", Head: "a", Risk: Result{Tier: TierLow}}
	med := Assessed{PR: "pr/2", Head: "b", Risk: Result{Tier: TierMedium}}
	hi := Assessed{PR: "pr/3", Head: "c", Risk: high("x")}
	for _, tc := range []struct {
		name string
		prs  []Assessed
		max  Tier
		ok   Approvals
		want int
	}{
		{"low stack under low", []Assessed{low, low}, TierLow, nil, -1},
		{"medium above low", []Assessed{low, med}, TierLow, nil, 1},
		{"medium under medium", []Assessed{low, med}, TierMedium, nil, -1},
		{"high above medium", []Assessed{med, hi}, TierMedium, nil, 1},
		{"high under high (the default)", []Assessed{hi, med, low}, TierHigh, nil, -1},
		{"first blocker reported", []Assessed{hi, med}, TierLow, nil, 0},
		{"approved at its head", []Assessed{low, hi}, TierLow, Approvals{"pr/3": "c"}, -1},
		{"approved at an old head", []Assessed{low, hi}, TierLow, Approvals{"pr/3": "old"}, 1},
		{"approval of another PR", []Assessed{hi}, TierLow, Approvals{"pr/1": "c"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Gate(tc.prs, tc.max, tc.ok); got != tc.want {
				t.Fatalf("Gate = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestApprovalNeedsAHead(t *testing.T) {
	if (Approvals{"pr/1": ""}).Approved("pr/1", "") {
		t.Fatal("an approval with no head approves nothing")
	}
}

func TestTierNoticeText(t *testing.T) {
	got := TierNotice("https://github.com/o/r/pull/42", high("migrations/0042.sql matches high glob **/migrations/**"))
	want := "PR #42 is high risk (migrations/0042.sql matches high glob **/migrations/**); approve with `saddle automerge approve 42`"
	if got != want {
		t.Fatalf("notice:\n got %q\nwant %q", got, want)
	}
}

func TestNewTierPolicyDefaultsToHigh(t *testing.T) {
	p, err := NewTierPolicy("", nil)
	if err != nil || p.Max != TierHigh {
		t.Fatalf("default max_tier = %v, %v; want high", p, err)
	}
	if p, err := NewTierPolicy("Low", nil); err != nil || p.Max != TierLow {
		t.Fatalf("max_tier low = %v, %v", p, err)
	}
	if _, err := NewTierPolicy("severe", nil); err == nil {
		t.Fatal("unknown max_tier: want error")
	}
}

// tiered gives the rig a max_tier policy with per-task tiers.
func (r *rig) tiered(max Tier, tiers map[string]Result) {
	r.w.Tiers = &TierPolicy{Max: max, Assess: AssessorFunc(func(task, _ string) (Result, error) {
		return tiers[task], nil
	})}
}

func (r *rig) tierNotices() []notice {
	var out []notice
	for _, n := range r.notices {
		if strings.Contains(n.text, "risk") {
			out = append(out, n)
		}
	}
	return out
}

func TestMaxTierLowMergesLowStack(t *testing.T) {
	r := newRig(t, "", "t1")
	r.on(t)
	r.tiered(TierLow, map[string]Result{"t1": {Tier: TierLow}})
	check(t, r.w)
	if !slices.Equal(r.gh.merged, []string{"pr/t1 squash"}) {
		t.Fatalf("low stack under max_tier low: merged %v", r.gh.merged)
	}
	if n := r.tierNotices(); len(n) != 0 {
		t.Fatalf("notices for a low stack: %+v", n)
	}
}

func TestMaxTierLowHoldsHighStackWithOneNotice(t *testing.T) {
	r := newRig(t, "", "t1", "t2")
	r.on(t)
	r.tiered(TierLow, map[string]Result{"t2": high("migrations/0042.sql matches high glob **/migrations/**")})
	for range 3 {
		st := check(t, r.w)
		if s := st.Stacks[0]; s.Ready || !strings.Contains(s.Why, "PR #t2 is high risk") {
			t.Fatalf("stack = %+v, want held for pr/t2's tier", s)
		}
	}
	if len(r.gh.merged) != 0 {
		t.Fatalf("a high PR anywhere in the stack holds all of it: merged %v", r.gh.merged)
	}
	want := "PR #t2 is high risk (migrations/0042.sql matches high glob **/migrations/**); approve with `saddle automerge approve t2`"
	if n := r.tierNotices(); len(n) != 1 || !n[0].action || n[0].text != want {
		t.Fatalf("notices = %+v, want exactly one needs-you notice %q", n, want)
	}
}

func TestApproveReleasesHighStack(t *testing.T) {
	r := newRig(t, "", "t1")
	r.on(t)
	r.tiered(TierLow, map[string]Result{"t1": high("auth/token.go matches high glob **/auth/**")})
	check(t, r.w)
	if len(r.gh.merged) != 0 {
		t.Fatal("merged a high PR under max_tier low")
	}
	prs, err := r.w.Approve("pr/t1")
	if err != nil || !slices.Equal(prs, []string{"pr/t1"}) {
		t.Fatalf("Approve = %v, %v", prs, err)
	}
	if !slices.Contains(r.kinds(), EventApproved) {
		t.Fatalf("no approve event: %+v", r.events)
	}
	check(t, r.w)
	if !slices.Equal(r.gh.merged, []string{"pr/t1 squash"}) {
		t.Fatalf("after approve: merged %v", r.gh.merged)
	}
}

func TestNewHeadClearsApproval(t *testing.T) {
	r := newRig(t, "", "t1")
	r.on(t)
	r.tiered(TierLow, map[string]Result{"t1": high("deploy/app.yaml matches high glob **/deploy/**")})
	check(t, r.w)
	if _, err := r.w.Approve("t1"); err != nil {
		t.Fatal(err)
	}
	r.gh.prs["pr/t1"].HeadSHA = "sha-t1-pushed"
	st := check(t, r.w)
	if len(r.gh.merged) != 0 || st.Stacks[0].Ready {
		t.Fatalf("a push after approve must clear it: merged %v, stack %+v", r.gh.merged, st.Stacks[0])
	}
	if n := r.tierNotices(); len(n) != 2 {
		t.Fatalf("the new head asks again, once: %+v", n)
	}
}

func TestApproveStackApprovesEveryPR(t *testing.T) {
	r := newRig(t, "", "t1", "t2")
	r.tiered(TierLow, map[string]Result{"t1": high("a"), "t2": high("b")})
	prs, err := r.w.Approve("t1")
	if err != nil || !slices.Equal(prs, []string{"pr/t1", "pr/t2"}) {
		t.Fatalf("Approve(stack) = %v, %v", prs, err)
	}
	r.on(t)
	check(t, r.w)
	if !slices.Equal(r.gh.merged, []string{"pr/t1 squash"}) {
		t.Fatalf("merged %v", r.gh.merged)
	}
}

func TestApproveUnknownRef(t *testing.T) {
	r := newRig(t, "", "t1")
	if _, err := r.w.Approve("t404"); err == nil {
		t.Fatal("approve of a PR not in any stack: want error")
	}
}

func TestAssessErrorFailsClosed(t *testing.T) {
	r := newRig(t, "", "t1")
	r.on(t)
	r.w.Tiers = &TierPolicy{Max: TierMedium, Assess: AssessorFunc(func(string, string) (Result, error) {
		return Result{Tier: TierLow}, errors.New("git diff failed")
	})}
	st := check(t, r.w)
	if len(r.gh.merged) != 0 || !strings.Contains(st.Stacks[0].Why, "git diff failed") {
		t.Fatalf("an assess error must count as high: merged %v, stack %+v", r.gh.merged, st.Stacks[0])
	}
}

func TestNoTierPolicyKeepsTodaysBehavior(t *testing.T) {
	r := newRig(t, "", "t1")
	r.on(t)
	check(t, r.w)
	if len(r.gh.merged) != 1 {
		t.Fatalf("no policy: merged %v", r.gh.merged)
	}
}
