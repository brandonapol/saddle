package prgraph

import (
	"fmt"
	"strconv"
	"time"
)

// ActionKind names an action (ADR 0009 §3).
type ActionKind string

// The action set.
const (
	// ActGate runs the repo's gate on Head onto Onto.
	ActGate ActionKind = "gate"
	// ActRebase rebases the change onto Onto; the new head comes back as a
	// new branch tip.
	ActRebase ActionKind = "rebase"
	// ActPublish opens the change's PR on Base.
	ActPublish ActionKind = "publish"
	// ActMove sets PR's base to Base and relinks it into Stack.
	ActMove ActionKind = "move"
	// ActLink links Stack natively on the forge.
	ActLink ActionKind = "link"
	// ActMerge merges PR with Method.
	ActMerge ActionKind = "merge"
	// ActNeedsFix asks for a fix of Cause on Change, or on Base when Change
	// is empty (base-red).
	ActNeedsFix ActionKind = "needs_fix"
	// ActClose closes PR.
	ActClose ActionKind = "close"
	// ActTransition writes the change's state row.
	ActTransition ActionKind = "transition"
	// ActEscalate tells a human once, keyed by Key; retries continue.
	ActEscalate ActionKind = "escalate"
	// ActWait does nothing until Until (or the next tick).
	ActWait ActionKind = "wait"
)

// Guard is what an effectful action expects of the world. The executor
// checks it against a fresh read and applies the action with a
// compare-and-set; a failed guard is "stale, skip", never an error. Empty
// fields aren't checked.
type Guard struct {
	Gen     int64  `json:"gen,omitempty"`      // the change row's gen
	Head    string `json:"head,omitempty"`     // the change branch's tip
	PRBase  string `json:"pr_base,omitempty"`  // the PR's current base
	BaseTip string `json:"base_tip,omitempty"` // base's tip
	// Deps are "id=head" for each unmerged dependency the decision read: the
	// store's CAS also requires each of those rows to still have that head
	// and not be merged or superseded. A transition decided from a
	// dependency's old head is then stale, not wrong.
	Deps []string `json:"deps,omitempty"`
}

// IsZero reports whether g checks nothing.
func (g Guard) IsZero() bool {
	return g.Gen == 0 && g.Head == "" && g.PRBase == "" && g.BaseTip == "" && len(g.Deps) == 0
}

// Action is one step of a Plan. Kind selects which fields matter.
type Action struct {
	Kind   ActionKind `json:"kind"`
	Change string     `json:"change,omitempty"`
	PR     int        `json:"pr,omitempty"`
	// Base: Publish/Move target; the base of a base-red NeedsFix.
	Base string `json:"base,omitempty"`
	Onto *Onto  `json:"onto,omitempty"`
	// From: a queue Move replays only From..head onto Onto (`git rebase
	// --onto <base> From`): From is the old base tip the change was cut on.
	From   string `json:"from,omitempty"`
	Stack  string `json:"stack,omitempty"`
	Method string `json:"method,omitempty"`
	// Cause: NeedsFix and Escalate.
	Cause string `json:"cause,omitempty"`
	// Reason: Close, Transition and Wait.
	Reason string      `json:"reason,omitempty"`
	To     ChangeState `json:"to,omitempty"`
	// Head: a Transition to queued records the branch's new head.
	Head     string    `json:"head,omitempty"`
	Deadline time.Time `json:"deadline,omitzero"`
	RetryAt  time.Time `json:"retry_at,omitzero"`
	Until    time.Time `json:"until,omitzero"`
	// Key: Escalate's dedupe key.
	Key string `json:"key,omitempty"`
	// Blocker: the change a Wait waits on (itself, or a dependency).
	Blocker string `json:"blocker,omitempty"`
	Guard   Guard  `json:"guard,omitzero"`
}

// onto is a.Onto, or the zero Onto when it has none.
func (a Action) onto() Onto {
	if a.Onto == nil {
		return Onto{}
	}
	return *a.Onto
}

// Effectful reports whether a changes anything (everything but Wait).
func (a Action) Effectful() bool { return a.Kind != ActWait }

// AttemptKey is the prgraph_attempt key for a: the action plus the inputs
// that, when they change, start a fresh attempt. NeedsFix on a change is
// keyed on the change alone, so the repair cap survives new heads.
func (a Action) AttemptKey() string {
	switch a.Kind {
	case ActGate, ActRebase:
		return string(a.Kind) + "/" + a.Change + "/" + a.Guard.Head + "/" + a.onto().depsKey()
	case ActPublish:
		return "publish/" + a.Change + "/" + a.Guard.Head + "/" + a.Base
	case ActMove:
		return "move/" + strconv.Itoa(a.PR) + "/" + a.Base
	case ActLink:
		return "link/" + a.Stack
	case ActMerge:
		return "merge/" + strconv.Itoa(a.PR) + "/" + a.Guard.Head
	case ActNeedsFix:
		if a.Change == "" {
			return "needs_fix_base/" + a.Base + "/" + a.Guard.BaseTip + "/" + a.Cause
		}
		return "needs_fix/" + a.Change
	case ActClose:
		return "close/" + strconv.Itoa(a.PR)
	case ActEscalate:
		return "escalate/" + a.Key
	case ActTransition:
		return "transition/" + a.Change
	}
	return string(a.Kind) + "/" + a.Change
}

func (a Action) String() string {
	switch a.Kind {
	case ActGate, ActRebase:
		return fmt.Sprintf("%s(%s, %s)", a.Kind, a.Change, a.onto().Key())
	case ActPublish:
		return fmt.Sprintf("publish(%s, %s)", a.Change, a.Base)
	case ActMove:
		return fmt.Sprintf("move(#%d, %s, stack %s)", a.PR, a.Base, a.Stack)
	case ActLink:
		return fmt.Sprintf("link(%s)", a.Stack)
	case ActMerge:
		return fmt.Sprintf("merge(#%d, %s)", a.PR, a.Method)
	case ActNeedsFix:
		if a.Change == "" {
			return fmt.Sprintf("needs_fix(base %s, %s)", a.Base, a.Cause)
		}
		return fmt.Sprintf("needs_fix(%s, %s)", a.Change, a.Cause)
	case ActClose:
		return fmt.Sprintf("close(#%d, %s)", a.PR, a.Reason)
	case ActTransition:
		return fmt.Sprintf("transition(%s, %s, %s)", a.Change, a.To, a.Reason)
	case ActEscalate:
		return fmt.Sprintf("escalate(%s, %s)", a.Key, a.Cause)
	case ActWait:
		return fmt.Sprintf("wait(%s, %s)", a.Change, a.Reason)
	}
	return string(a.Kind)
}

// Plan is Reconcile's output: ordered, applied one at a time.
type Plan struct {
	Actions []Action `json:"actions"`
}

// Effectful returns the plan's actions that change something.
func (p Plan) Effectful() []Action {
	var out []Action
	for _, a := range p.Actions {
		if a.Effectful() {
			out = append(out, a)
		}
	}
	return out
}

// OutcomeKind classifies what happened when the executor applied an action.
type OutcomeKind string

// Outcome kinds. Forge errors are waits (transient, rate-limited), never
// failures.
const (
	OutcomeOK            OutcomeKind = "ok"
	OutcomeStale         OutcomeKind = "stale" // guard failed: skip
	OutcomeTransient     OutcomeKind = "transient"
	OutcomeRateLimited   OutcomeKind = "rate_limited"
	OutcomeRefused       OutcomeKind = "refused"
	OutcomeDeterministic OutcomeKind = "deterministic"
)

// waiting reports whether k is a forge wait rather than a failure.
func (k OutcomeKind) waiting() bool { return k == OutcomeTransient || k == OutcomeRateLimited }

// failed reports whether k counts toward escalation.
func (k OutcomeKind) failed() bool { return k == OutcomeRefused || k == OutcomeDeterministic }

// Outcome is the executor's classified result.
type Outcome struct {
	Kind OutcomeKind `json:"kind"`
	// Until: rate-limited until.
	Until  time.Time `json:"until,omitzero"`
	Reason string    `json:"reason,omitempty"`
}
