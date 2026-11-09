package prgraph

import (
	"sort"
	"strings"
	"time"
)

// ChangeState is a change's place in the state machine (ADR 0009 §2).
type ChangeState string

// The change states. merged and superseded are terminal.
const (
	// Queued: submitted; this head hasn't passed the gate on base + deps.
	Queued ChangeState = "queued"
	// Landed: gate green on base + deps; PR not open on its desired base yet.
	Landed ChangeState = "landed"
	// Published: PR open on its desired base; required checks pending.
	Published ChangeState = "published"
	// Green: required, non-host checks green at the head on its desired base.
	Green ChangeState = "green"
	// Red: a required non-host check, the gate, a rebase or a forge rule is
	// red for this change alone. Reason holds the cause.
	Red ChangeState = "red"
	// Merged: base contains the work. Terminal.
	Merged ChangeState = "merged"
	// Superseded: left for good. Reason is one of the Superseded* reasons.
	// Terminal.
	Superseded ChangeState = "superseded"
)

// Terminal reports whether s is merged or superseded.
func (s ChangeState) Terminal() bool { return s == Merged || s == Superseded }

// Reasons a change is superseded.
const (
	SupersededClosed    = "closed"
	SupersededKilled    = "killed"
	SupersededUnstacked = "unstacked"
	SupersededGaveUp    = "gave_up"
)

// Red causes Reconcile produces. A failing check is "check:<name>".
const (
	CauseGate        = "gate"
	CauseConflict    = "conflict"
	CausePublish     = "publish"
	CauseChecksStuck = "checks_stuck"
	CauseMerge       = "merge"
	causeCheckPrefix = "check:"
)

// CheckCause is the red cause for a failing check.
func CheckCause(name string) string { return causeCheckPrefix + name }

// Change is one row of prgraph_change: intent plus the state machine's clock.
type Change struct {
	ID     string `json:"id"`
	Branch string `json:"branch"`
	// Base is the branch the work finally goes to (e.g. main).
	Base string `json:"base"`
	// Head is the work the host submitted (intent).
	Head string `json:"head"`
	// Seq is submission order: a tie-break only, never a dependency.
	Seq int64 `json:"seq"`
	// PR is a cache; the forge snapshot wins.
	PR       int         `json:"pr,omitempty"`
	State    ChangeState `json:"state"`
	Reason   string      `json:"reason,omitempty"`
	Since    time.Time   `json:"since,omitzero"`
	Deadline time.Time   `json:"deadline,omitzero"`
	RetryAt  time.Time   `json:"retry_at,omitzero"`
	// Automerge off makes the owner the merger; green keeps its deadline.
	Automerge bool `json:"automerge"`
	// HoldUntil is a queue or automerge hold. It always has a time.
	HoldUntil time.Time `json:"hold_until,omitzero"`
	// Gen is the CAS version for every write to the row.
	Gen int64 `json:"gen"`
}

// Edge is one row of prgraph_edge: Change declared `after: Dep` (ADR 0012
// rule 8). It is the only kind of edge: nothing is inferred from a parent,
// an issue, free text, a hand retarget or the gate. A change has at
// most one unmerged dependency; Reconcile waits rather than stack a change
// with two.
type Edge struct {
	Change string `json:"change"`
	Dep    string `json:"dep"`
}

// Attempt is one row of prgraph_attempt (#826): the retry history of one
// action key. Inputs are part of the key, so a new head or base starts a
// fresh attempt.
type Attempt struct {
	Key    string `json:"key"`
	Kind   string `json:"kind"`
	Inputs string `json:"inputs,omitempty"`
	// N counts outcomes that were not forge waits.
	N int `json:"n"`
	// Waits counts consecutive transient or rate-limited outcomes.
	Waits int `json:"waits,omitempty"`
	// Last is the kind of the latest outcome.
	Last      OutcomeKind `json:"last"`
	NextAt    time.Time   `json:"next_at,omitzero"`
	FirstErr  string      `json:"first_err,omitempty"`
	LastErr   string      `json:"last_err,omitempty"`
	Escalated bool        `json:"escalated,omitempty"`
}

// Policy is the host's configuration. Zero fields take DefaultPolicy's.
type Policy struct {
	QueuedDeadline    time.Duration `json:"queued_deadline,omitempty"`
	LandedDeadline    time.Duration `json:"landed_deadline,omitempty"`
	PublishedDeadline time.Duration `json:"published_deadline,omitempty"`
	GreenDeadline     time.Duration `json:"green_deadline,omitempty"`
	// RepairAttempts caps NeedsFix per change ([ci] repair_attempts).
	RepairAttempts int `json:"repair_attempts,omitempty"`
	// FixWait is how long a NeedsFix gets before the next one.
	FixWait time.Duration `json:"fix_wait,omitempty"`
	// Settle is how long after a Publish, Move or Rebase check results are
	// treated as stale.
	Settle time.Duration `json:"settle,omitempty"`
	// RetryBase and RetryMax bound the backoff for failed actions.
	RetryBase time.Duration `json:"retry_base,omitempty"`
	RetryMax  time.Duration `json:"retry_max,omitempty"`
	// EscalateAfter is how many deterministic or refused failures of one
	// action key escalate it (once; retries continue).
	EscalateAfter int    `json:"escalate_after,omitempty"`
	MergeMethod   string `json:"merge_method,omitempty"`
	// HostChecks are check names the host itself posts.
	// They are never a reason to merge, to wait or to fix.
	HostChecks []string `json:"host_checks,omitempty"`
}

// DefaultPolicy returns ADR 0009 §2's defaults.
func DefaultPolicy() Policy {
	return Policy{
		QueuedDeadline:    2 * time.Hour,
		LandedDeadline:    30 * time.Minute,
		PublishedDeadline: 3 * time.Hour,
		GreenDeadline:     time.Hour,
		RepairAttempts:    3,
		FixWait:           time.Hour,
		Settle:            5 * time.Minute,
		RetryBase:         time.Minute,
		RetryMax:          30 * time.Minute,
		EscalateAfter:     3,
		MergeMethod:       "squash",
	}
}

func (p Policy) withDefaults() Policy {
	d := DefaultPolicy()
	if p.QueuedDeadline <= 0 {
		p.QueuedDeadline = d.QueuedDeadline
	}
	if p.LandedDeadline <= 0 {
		p.LandedDeadline = d.LandedDeadline
	}
	if p.PublishedDeadline <= 0 {
		p.PublishedDeadline = d.PublishedDeadline
	}
	if p.GreenDeadline <= 0 {
		p.GreenDeadline = d.GreenDeadline
	}
	if p.RepairAttempts <= 0 {
		p.RepairAttempts = d.RepairAttempts
	}
	if p.FixWait <= 0 {
		p.FixWait = d.FixWait
	}
	if p.Settle <= 0 {
		p.Settle = d.Settle
	}
	if p.RetryBase <= 0 {
		p.RetryBase = d.RetryBase
	}
	if p.RetryMax <= 0 {
		p.RetryMax = d.RetryMax
	}
	if p.EscalateAfter <= 0 {
		p.EscalateAfter = d.EscalateAfter
	}
	if p.MergeMethod == "" {
		p.MergeMethod = d.MergeMethod
	}
	return p
}

// State is what the host stores: changes, edges, attempts and policy.
type State struct {
	Changes  []Change  `json:"changes"`
	Edges    []Edge    `json:"edges"`
	Attempts []Attempt `json:"attempts"`
	Policy   Policy    `json:"policy"`
}

// PRState is a pull request's state on the forge.
type PRState string

// PR states.
const (
	PROpen   PRState = "open"
	PRClosed PRState = "closed"
	PRMerged PRState = "merged"
)

// Mergeability is the forge's answer to "can this PR merge".
type Mergeability string

// Mergeability values.
const (
	MergeUnknown     Mergeability = ""
	MergeClean       Mergeability = "clean"
	MergeConflicting Mergeability = "conflicting"
	MergeBlocked     Mergeability = "blocked"
)

// PR is one pull request in the snapshot.
type PR struct {
	Number int     `json:"number"`
	Branch string  `json:"branch"` // head ref
	Head   string  `json:"head"`   // head sha
	Base   string  `json:"base"`   // base ref
	State  PRState `json:"state"`
	// StackBelow is the PR below this one in the forge's native stack; 0 is
	// the bottom or no native stack.
	StackBelow int          `json:"stack_below,omitempty"`
	Mergeable  Mergeability `json:"mergeable,omitempty"`
}

// CheckStatus is one check run's result.
type CheckStatus string

// Check statuses.
const (
	CheckPending CheckStatus = "pending"
	CheckSuccess CheckStatus = "success"
	CheckFailure CheckStatus = "failure"
)

// Check is one check result at a head (prgraph_check).
type Check struct {
	Head      string      `json:"head"`
	Name      string      `json:"name"`
	Status    CheckStatus `json:"status"`
	Required  bool        `json:"required"`
	HostOwned bool        `json:"host_owned,omitempty"`
}

// RequiredCheck is a check a base branch requires (branch protection). A
// required check with no result at a head is pending there.
type RequiredCheck struct {
	Base string `json:"base"`
	Name string `json:"name"`
}

// ForgeSnapshot is the forge's actual state, read every tick.
type ForgeSnapshot struct {
	PRs      []PR            `json:"prs"`
	Checks   []Check         `json:"checks"`
	Required []RequiredCheck `json:"required,omitempty"`
	// NativeStacks says the forge links stacks natively (Link applies).
	NativeStacks bool `json:"native_stacks,omitempty"`
	// Degraded means the forge answered transient or rate-limited and this
	// snapshot may be old. Clocks pause and forge writes wait until
	// DegradedUntil.
	Degraded      bool      `json:"degraded,omitempty"`
	DegradedUntil time.Time `json:"degraded_until,omitzero"`
}

// BranchHead is a branch's tip in git.
type BranchHead struct {
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
}

// GateStatus is a gate run's result.
type GateStatus string

// Gate statuses.
const (
	GatePass     GateStatus = "pass"
	GateFail     GateStatus = "fail"
	GateConflict GateStatus = "conflict"
)

// Onto is what a gate or rebase targets: base tip plus the heads of the
// change's unmerged dependencies, and nothing else.
type Onto struct {
	Base    string   `json:"base"`
	BaseTip string   `json:"base_tip"`
	Deps    []string `json:"deps,omitempty"` // dependency heads, sorted
}

// Key is Onto's stable text form, prgraph_gate.onto.
func (o Onto) Key() string {
	return o.Base + "@" + o.BaseTip + "+" + strings.Join(o.Deps, ",")
}

// depsKey is Onto without the base tip: a gate result stays good when
// base moves on under it.
func (o Onto) depsKey() string { return o.Base + "+" + strings.Join(o.Deps, ",") }

// GateResult is one row of prgraph_gate: an immutable fact.
type GateResult struct {
	Head   string     `json:"head"`
	Onto   Onto       `json:"onto"`
	Status GateStatus `json:"status"`
	Detail string     `json:"detail,omitempty"`
	RanAt  time.Time  `json:"ran_at,omitzero"`
}

// GitFacts is git's actual state, read every tick.
type GitFacts struct {
	// Bases are base branch tips (e.g. main).
	Bases []BranchHead `json:"bases"`
	// Heads are change branch tips. A tip that differs from Change.Head is a
	// new head.
	Heads []BranchHead `json:"heads"`
	// InBase lists changes whose work base contains (patch-ids or trailer).
	InBase []string     `json:"in_base"`
	Gates  []GateResult `json:"gates"`
}

// Input is everything Reconcile sees.
type Input struct {
	// Now is the only clock Reconcile sees.
	Now   time.Time     `json:"now"`
	State State         `json:"state"`
	Forge ForgeSnapshot `json:"forge"`
	Git   GitFacts      `json:"git"`
}

// ChangeRef names a change for the Source.
type ChangeRef struct {
	ID     string `json:"id"`
	Branch string `json:"branch"`
	Base   string `json:"base"`
	PR     int    `json:"pr,omitempty"`
}

// sortedCopy returns a sorted copy of ss.
func sortedCopy(ss []string) []string {
	out := append([]string(nil), ss...)
	sort.Strings(out)
	return out
}
