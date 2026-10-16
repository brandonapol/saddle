package automerge

// The max_tier gate (#332). [automerge] max_tier caps the risk tier that
// merges without a human: a stack auto-merges only when every PR in it is at
// or below max_tier, or approved at its current head. A stack above it is
// treated like a hold, and the orchestrator hears about it once per head
// with a needs-you notice naming the approve command. An approval is keyed
// by PR and head SHA, so a new push clears it. The default max_tier, high,
// lets everything through, as before.

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/forge"
)

// EventApproved is a PR approved past the max_tier gate, at a head.
const EventApproved = "automerge_approved"

// DefaultMaxTier lets every tier merge: today's behavior.
const DefaultMaxTier = TierHigh

// Assessor gives the risk tier of a task's PR at head.
type Assessor interface {
	Assess(task, head string) (Result, error)
}

// AssessorFunc adapts a function to Assessor.
type AssessorFunc func(task, head string) (Result, error)

// Assess calls f.
func (f AssessorFunc) Assess(task, head string) (Result, error) { return f(task, head) }

// TierPolicy is the max_tier gate. A nil *TierPolicy, or one with no
// Assess, gates nothing. Note the zero Max is TierLow, the strictest; build
// one from config with NewTierPolicy.
type TierPolicy struct {
	Max    Tier
	Assess Assessor
}

// NewTierPolicy parses [automerge] max_tier; "" is DefaultMaxTier.
func NewTierPolicy(maxTier string, a Assessor) (*TierPolicy, error) {
	p := &TierPolicy{Max: DefaultMaxTier, Assess: a}
	if strings.TrimSpace(maxTier) == "" {
		return p, nil
	}
	t, err := ParseTier(maxTier)
	if err != nil {
		return nil, fmt.Errorf("[automerge] max_tier: %w", err)
	}
	p.Max = t
	return p, nil
}

// Approvals maps a PR to the head SHA it was approved at.
type Approvals map[string]string

// Approved says whether pr is approved at head.
func (a Approvals) Approved(pr, head string) bool {
	h, ok := a[pr]
	return ok && head != "" && h == head
}

// Assessed is one PR of a stack with its tier.
type Assessed struct {
	PR   string
	Head string
	Risk Result
}

// Gate returns the index of the first PR in prs that is above max and not
// approved at its head, or -1 when the stack may auto-merge.
func Gate(prs []Assessed, max Tier, ok Approvals) int {
	return slices.IndexFunc(prs, func(p Assessed) bool {
		return p.Risk.Tier > max && !ok.Approved(p.PR, p.Head)
	})
}

// TierNotice is the needs-you text for a PR the gate holds. A GitLab MR is
// "MR !N", and approved by that.
func TierNotice(pr string, r Result) string {
	what, n := "PR #"+prNumber(pr), prNumber(pr)
	if s := forge.PRShort(pr); strings.HasPrefix(s, "!") {
		what, n = "MR "+s, s
	}
	why := strings.Join(r.Reasons, "; ")
	if why == "" {
		why = "no reason given"
	}
	return fmt.Sprintf("%s is %s risk (%s); approve with `saddle automerge approve %s`", what, r.Tier, why, n)
}

// prNumber is the last path element of a PR URL: its number.
func prNumber(pr string) string {
	return pr[strings.LastIndex(pr, "/")+1:]
}

// assess sets n's tier. An Assess error fails closed: the PR counts as high.
func (w *Watcher) assess(n *Node) {
	if w.Tiers == nil || w.Tiers.Assess == nil || n.Error != "" {
		return
	}
	r, err := w.Tiers.Assess.Assess(n.Task, n.head)
	if err != nil {
		r = Result{Tier: TierHigh, Reasons: []string{"its risk couldn't be assessed: " + err.Error()}}
	}
	n.Tier, n.TierReasons, n.risk = r.Tier.String(), r.Reasons, &r
}

// gate holds every stack that would merge (ready or collapsible) but has a
// PR above max_tier that isn't approved at its head.
func (w *Watcher) gate(stacks []Stack, ok Approvals) {
	if w.Tiers == nil || w.Tiers.Assess == nil {
		return
	}
	for i := range stacks {
		x := &stacks[i]
		prs := make([]Assessed, len(x.Nodes))
		for j := range x.Nodes {
			n := &x.Nodes[j]
			n.Approved = ok.Approved(n.PR, n.head)
			prs[j] = Assessed{PR: n.PR, Head: n.head}
			if n.risk != nil {
				prs[j].Risk = *n.risk
			}
		}
		if !x.Ready && x.Collapse == "" {
			continue
		}
		j := Gate(prs, w.Tiers.Max, ok)
		if j < 0 {
			continue
		}
		x.Ready, x.Collapse, x.wait = false, "", false
		x.Gated, x.gatedHead = prs[j].PR, prs[j].Head
		x.Why = TierNotice(prs[j].PR, prs[j].Risk)
	}
}

// noticeTiers sends the needs-you notice for each gated PR once per head.
func (w *Watcher) noticeTiers(s *State, stacks []Stack) {
	open := map[string]bool{}
	for _, x := range stacks {
		for _, n := range x.Nodes {
			open[n.PR] = true
		}
	}
	for pr := range s.TierNoticed {
		if !open[pr] {
			delete(s.TierNoticed, pr)
		}
	}
	for _, x := range stacks {
		if x.Gated == "" || s.TierNoticed[x.Gated] == x.gatedHead {
			continue
		}
		if s.TierNoticed == nil {
			s.TierNoticed = map[string]string{}
		}
		s.TierNoticed[x.Gated] = x.gatedHead
		w.notify(true, x.Why)
	}
}

// Approve lets PRs past the max_tier gate at their current heads: every PR
// of the stack when ref is a stack's ID, else the one PR ref names (task,
// PR or MR URL, or number: "#12", "!12"). It reads the forge for the heads;
// a push after this clears the approval. It returns the PRs approved.
func (w *Watcher) Approve(ref string) ([]string, error) {
	st, err := w.Plan()
	if err != nil {
		return nil, fmt.Errorf("reading the PRs' heads: %w", err)
	}
	num := strings.TrimPrefix(ref, "#")
	var pick []Node
	for _, x := range st.Stacks {
		if x.ID == ref {
			pick = x.Nodes
			break
		}
		for _, n := range x.Nodes {
			if n.Task == ref || n.PR == ref || prNumber(n.PR) == num || forge.SamePR(n.PR, ref) {
				pick = append(pick, n)
			}
		}
	}
	if len(pick) == 0 {
		return nil, fmt.Errorf("%s isn't an open PR or stack", ref)
	}
	for _, n := range pick {
		if n.head == "" {
			return nil, errors.New("can't read the head of " + n.PR + ": " + n.Error)
		}
	}
	var prs []string
	if _, err := update(w.Path, func(s *State) error {
		if s.Approved == nil {
			s.Approved = Approvals{}
		}
		for _, n := range pick {
			s.Approved[n.PR] = n.head
			prs = append(prs, n.PR)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	for _, n := range pick {
		w.event(n.Task, EventApproved, n.PR+" at "+n.head)
	}
	return prs, nil
}
