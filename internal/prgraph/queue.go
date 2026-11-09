package prgraph

import (
	"sort"
	"strings"
	"time"
)

// The merge queue (ADR 0012 rule 5, #935). Every change here already has its
// one PR open: the host published it on done. Queue decides what to do next
// with those PRs, and the host applies one action per tick:
//
//   - a PR that merged on the forge is a Transition to merged;
//   - base red on its own required checks pauses everything: one Escalate,
//     then waits until base is green;
//   - a PR stacked on its declared after, once that after has merged, is a
//     Move: `git rebase --onto <base> <old base tip>`, force-push, retarget
//     to base;
//   - the candidates are the open PRs on base that aren't drafts, aren't
//     labeled needs-human, aren't held by the owner and weren't left for
//     their author to fix, in owner order and then oldest first. Red CI
//     skips a candidate; one behind base or conflicting is a Rebase onto
//     base; one green and mergeable is a Merge with the repo's method.
//
// Queue is pure and deterministic like Reconcile: the same QueueInput gives
// the same Plan whatever the order of its slices.

// Wait reasons Queue gives.
const (
	WaitQueueRed      = "red"
	WaitQueueDraft    = "draft"
	WaitQueueHuman    = "needs_human"
	WaitQueueFix      = "fix_in_place"
	WaitQueueOff      = "merging_off"
	WaitQueueBlocked  = "blocked"
	WaitQueueAfter    = "after_unmerged"
	WaitQueueNotOpen  = "pr_not_open"
	WaitQueueStackOff = "base_not_after"
)

// QueueChange is one published change as the queue sees it: the host's
// intent (branch, declared after, owner order and hold) and what the forge
// and git say about its PR.
type QueueChange struct {
	ID string `json:"id"`
	// Seq is submission order: oldest first, after owner order.
	Seq int64 `json:"seq"`
	// Order is the owner's position for it, 1 first; 0 is none (after every
	// change with one).
	Order  int    `json:"order,omitempty"`
	Branch string `json:"branch"`
	// Head is the PR's head commit.
	Head string `json:"head"`
	// After is the change it declared after, while it is stacked on it
	// ("" when it targets base). AfterBranch is that change's branch and
	// AfterMerged says base has its work.
	After       string `json:"after,omitempty"`
	AfterBranch string `json:"after_branch,omitempty"`
	AfterMerged bool   `json:"after_merged,omitempty"`
	// From is the old base tip the change was cut on (the after branch's tip
	// under it): a Move replays only From..Head onto base.
	From       string       `json:"from,omitempty"`
	PR         int          `json:"pr"`
	PRState    PRState      `json:"pr_state"`
	PRBase     string       `json:"pr_base"`
	Draft      bool         `json:"draft,omitempty"`
	NeedsHuman bool         `json:"needs_human,omitempty"`
	Held       bool         `json:"held,omitempty"`
	Mergeable  Mergeability `json:"mergeable,omitempty"`
	// Behind: the forge says the PR must be brought up to date with its
	// base before it can merge.
	Behind bool `json:"behind,omitempty"`
	// Checks is the PR's check rollup at Head; "" when it has none.
	Checks CheckStatus `json:"checks,omitempty"`
	// FixInPlace is why the queue left this head for its author to fix (a
	// conflicting rebase). A new head clears it.
	FixInPlace string `json:"fix_in_place,omitempty"`
	// PushedAt is when the queue last pushed this head. Until Settle has
	// passed, a PR with no checks may only not have started its CI yet.
	PushedAt time.Time `json:"pushed_at,omitzero"`
}

// QueueInput is everything Queue sees.
type QueueInput struct {
	Now     time.Time `json:"now"`
	Base    string    `json:"base"`
	BaseTip string    `json:"base_tip"`
	// BaseRed names the base's own required checks that are red at its tip.
	BaseRed []string `json:"base_red,omitempty"`
	// Paused says the owner was already told base is red.
	Paused bool `json:"paused,omitempty"`
	// Merging is the owner's switch: off, nothing merges (rebases and
	// retargets still happen).
	Merging bool          `json:"merging"`
	Method  string        `json:"method"`
	Settle  time.Duration `json:"settle"`
	Changes []QueueChange `json:"changes"`
}

// QueueBaseRedKey is the Escalate key of a red base.
func QueueBaseRedKey(base string) string { return "base_red/" + base }

// Queue returns the merge queue's plan. Effectful actions come first, in the
// order the host should try them; it applies the first one that isn't
// backing off, and one only.
func Queue(in QueueInput) Plan {
	cs := append([]QueueChange(nil), in.Changes...)
	sort.Slice(cs, func(i, j int) bool { return queueLess(cs[i], cs[j]) })
	var trans, esc, moves, cands, waits []Action
	wait := func(c QueueChange, reason, blocker string) {
		waits = append(waits, Action{Kind: ActWait, Change: c.ID, PR: c.PR, Reason: reason, Blocker: blocker})
	}
	var open []QueueChange
	for _, c := range cs {
		switch c.PRState {
		case PRMerged:
			trans = append(trans, Action{Kind: ActTransition, Change: c.ID, PR: c.PR, To: Merged, Reason: "pr_merged",
				Guard: Guard{Head: c.Head}})
		case PROpen:
			open = append(open, c)
		default:
			wait(c, WaitQueueNotOpen, "")
		}
	}
	if len(in.BaseRed) > 0 {
		if !in.Paused {
			esc = append(esc, Action{Kind: ActEscalate, Base: in.Base, Key: QueueBaseRedKey(in.Base),
				Cause: "base_red: " + strings.Join(sortedCopy(in.BaseRed), ", "), Guard: Guard{BaseTip: in.BaseTip}})
		}
		for _, c := range open {
			wait(c, WaitBaseRed, "")
		}
		return queuePlan(trans, esc, nil, nil, waits)
	}
	onto := &Onto{Base: in.Base, BaseTip: in.BaseTip}
	for _, c := range open {
		g := Guard{Head: c.Head, PRBase: c.PRBase, BaseTip: in.BaseTip}
		if c.PRBase != in.Base {
			switch {
			case c.After == "" || c.PRBase != c.AfterBranch:
				// Not stacked on its declared after: not the queue's to move.
				wait(c, WaitQueueStackOff, "")
			case !c.AfterMerged:
				wait(c, WaitQueueAfter, c.After)
			case c.FixInPlace != "":
				wait(c, WaitQueueFix, c.ID)
			default:
				moves = append(moves, Action{Kind: ActMove, Change: c.ID, PR: c.PR, Base: in.Base, Onto: onto,
					From: c.From, Reason: "after_merged", Guard: g})
			}
			continue
		}
		switch {
		case c.Draft:
			wait(c, WaitQueueDraft, "")
			continue
		case c.NeedsHuman:
			wait(c, WaitQueueHuman, "")
			continue
		case c.Held:
			wait(c, WaitHold, c.ID)
			continue
		case c.FixInPlace != "":
			wait(c, WaitQueueFix, c.ID)
			continue
		case !in.Merging:
			wait(c, WaitQueueOff, "")
			continue
		case c.Checks == CheckFailure:
			// Red CI is the PR's own to fix; the queue moves on.
			wait(c, WaitQueueRed, c.ID)
			continue
		case c.Behind || c.Mergeable == MergeConflicting:
			cands = append(cands, Action{Kind: ActRebase, Change: c.ID, PR: c.PR, Onto: onto, Reason: queueRebaseWhy(c), Guard: g})
			continue
		case c.Checks == CheckPending:
			wait(c, WaitChecksPending, "")
			continue
		case c.Checks == "" && !c.PushedAt.IsZero() && in.Now.Before(c.PushedAt.Add(in.Settle)):
			wait(c, WaitSettling, "")
			continue
		case c.Mergeable == MergeUnknown:
			wait(c, WaitMergeable, "")
			continue
		case c.Mergeable == MergeBlocked:
			wait(c, WaitQueueBlocked, "")
			continue
		}
		cands = append(cands, Action{Kind: ActMerge, Change: c.ID, PR: c.PR, Method: in.Method, Guard: g})
	}
	return queuePlan(trans, esc, moves, cands, waits)
}

func queueRebaseWhy(c QueueChange) string {
	if c.Mergeable == MergeConflicting {
		return "conflicting"
	}
	return "behind"
}

// queueLess is queue order: owner order first, then oldest, then id.
func queueLess(a, b QueueChange) bool {
	oa, ob := a.Order, b.Order
	if (oa > 0) != (ob > 0) {
		return oa > 0
	}
	if oa != ob {
		return oa < ob
	}
	if a.Seq != b.Seq {
		return a.Seq < b.Seq
	}
	return a.ID < b.ID
}

func queuePlan(groups ...[]Action) Plan {
	p := Plan{Actions: []Action{}}
	for _, g := range groups {
		p.Actions = append(p.Actions, g...)
	}
	return p
}

// QueueKey is the retry key of a queue action: the action and its change.
// The host's retry inputs (head and base tip) reset it when they move.
func QueueKey(a Action) string {
	switch a.Kind {
	case ActEscalate:
		return "queue:escalate:" + a.Key
	}
	return "queue:" + string(a.Kind) + ":" + a.Change
}
