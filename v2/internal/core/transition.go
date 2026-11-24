package core

import (
	"fmt"
	"slices"
	"strings"
)

// Kind names an event: something that happened to a task.
type Kind string

// Every event the state machine knows.
const (
	SlotGranted   Kind = "slot_granted"   // the scheduler gave it a slot
	Waiting       Kind = "waiting"        // the scheduler says why it can't start; Arg is the reason
	TurnEnded     Kind = "turn_ended"     // the agent process exited; Arg is the outcome
	Finished      Kind = "finished"       // the worker called finish; Arg is the status
	Asked         Kind = "asked"          // the agent asked a question
	Answered      Kind = "answered"       // its question was answered
	Message       Kind = "message"        // a message was put in its inbox
	Published     Kind = "published"      // saddle opened or updated the PR
	PublishFailed Kind = "publish_failed" // publishing stopped; Arg is the step that failed
	PRObserved    Kind = "pr_observed"    // what the forge says about the PR
	Kill          Kind = "kill"
	Retry         Kind = "retry"
	Hold          Kind = "hold"
	Release       Kind = "release"
)

// Args lists the values Arg may take for each kind that takes one. Waiting
// takes any reason a ready task may carry.
var Args = map[Kind][]string{
	TurnEnded:     {"ok", "crash", "timeout", "quota", "auth", "interrupted"},
	Finished:      {"done", "no_change", "blocked"},
	PublishFailed: {"check", "conflict", "rules"},
	// checks: required checks are running. review and queue: they are green,
	// and the PR waits for a review or for its turn to merge.
	PRObserved: {"merged", "closed", "red", "conflicting", "checks", "review", "queue"},
}

// Kinds is every kind, in the order the table prints them.
var Kinds = []Kind{SlotGranted, Waiting, TurnEnded, Finished, Asked, Answered, Message,
	Published, PublishFailed, PRObserved, Kill, Retry, Hold, Release}

// Commands are the events the owner or the orchestrator causes directly.
// Every state that isn't terminal must have one that leaves it.
var Commands = []Kind{Answered, Retry, Release, Kill}

// DefaultFixCap is how many fixes a task gets before it gives up.
const DefaultFixCap = 2

// Event is one thing that happened. It carries everything Transition needs
// that isn't on the task, so Transition reads no settings and no clock.
type Event struct {
	Kind   Kind
	Arg    string
	PR     int64 // Published: the PR's number
	FixCap int   // the [merge] fix_attempts setting; 0 means DefaultFixCap
}

func (e Event) String() string {
	if e.Arg == "" {
		return string(e.Kind)
	}
	return fmt.Sprintf("%s(%s)", e.Kind, e.Arg)
}

// Effect is something the engine must do because of a transition. Effects
// are data: applying one twice is harmless.
type Effect string

// The effects.
const (
	StartTurn        Effect = "start_turn"        // run the agent on its pending messages
	StopTurn         Effect = "stop_turn"         // stop the running agent process
	Publish          Effect = "publish"           // check, rebase, validate, push, open the PR
	RemoveWorktree   Effect = "remove_worktree"   // and the local branch, if merged
	ClosePR          Effect = "close_pr"          // if there is one
	LabelNeedsHuman  Effect = "label_needs_human" // on the PR
	NotifyOwner      Effect = "notify_owner"
	WakeOrchestrator Effect = "wake_orchestrator"
)

// Rule is one row of the table: for a task that matches From, FromReason
// and Role, the event On with Arg moves it to To with Reason.
type Rule struct {
	From State
	// FromReason: "" matches any reason, "!x" any but x, otherwise exactly.
	FromReason string
	Role       string // "" matches any role
	On         Kind
	Arg        string // "" matches any; "a|b" matches a or b
	Unheld     bool   // the rule does not apply to a held task
	To         State  // "" leaves state and reason as they are
	Reason     string // ArgReason takes the event's Arg
	Fix        int    // +1 counts a fix, -1 resets the count
	Hold       int    // +1 sets held, -1 clears it
	Effects    []Effect
}

// ArgReason as a rule's Reason means "the event's Arg".
const ArgReason = "<arg>"

const turn = "!publishing" // a working task that has, or is about to have, a turn

// Table is the whole state machine. The first matching rule wins. An event
// with no matching rule is illegal and changes nothing.
//
// Two things apply to every rule and are not repeated in the rows:
//   - a rule with Fix +1 on a task already at the cap goes to blocked:
//     gave_up instead, with label_needs_human and notify_owner;
//   - a held task that lands in ready or pr_open has the reason held.
var Table = []Rule{
	// ready
	{From: Ready, On: SlotGranted, Unheld: true, To: Working, Reason: "new", Effects: []Effect{StartTurn}},
	{From: Ready, On: Waiting, To: Ready, Reason: ArgReason},
	{From: Ready, On: Message},
	{From: Ready, On: Hold, Hold: +1},
	{From: Ready, On: Release, To: Ready, Reason: "slot", Hold: -1},
	{From: Ready, On: Kill, To: Closed, Reason: "killed", Effects: []Effect{RemoveWorktree, ClosePR}},
	{From: Ready, On: PRObserved, Arg: "merged", To: Merged, Fix: -1, Effects: []Effect{RemoveWorktree, WakeOrchestrator}},
	{From: Ready, On: PRObserved, Arg: "closed", To: Closed, Reason: "pr_closed", Effects: []Effect{RemoveWorktree, WakeOrchestrator}},
	{From: Ready, On: TurnEnded},

	// working: publishing (the turn is over and saddle is publishing)
	{From: Working, FromReason: "publishing", On: Published, To: PROpen, Reason: "checks"},
	{From: Working, FromReason: "publishing", On: PublishFailed, To: Working, Reason: ArgReason, Fix: +1, Effects: []Effect{StartTurn}},
	{From: Working, FromReason: "publishing", On: TurnEnded},

	// working, with a turn
	{From: Working, FromReason: turn, Role: Orchestrator, On: TurnEnded, Arg: "ok", To: Ready, Reason: "wake"},
	{From: Working, FromReason: turn, Role: Orchestrator, On: Asked, To: Blocked, Reason: "question", Effects: []Effect{NotifyOwner}},
	{From: Working, FromReason: "unfinished", On: TurnEnded, Arg: "ok|crash|timeout", To: Blocked, Reason: "turn_failed", Effects: []Effect{NotifyOwner, WakeOrchestrator}},
	{From: Working, FromReason: turn, On: TurnEnded, Arg: "ok|crash|timeout", To: Working, Reason: "unfinished", Effects: []Effect{StartTurn}},
	{From: Working, FromReason: turn, On: TurnEnded, Arg: "quota", To: Ready, Reason: "no_provider"},
	{From: Working, FromReason: turn, On: TurnEnded, Arg: "auth", To: Blocked, Reason: "turn_failed", Effects: []Effect{NotifyOwner}},
	{From: Working, FromReason: turn, On: TurnEnded, Arg: "interrupted", To: Working, Reason: "message", Effects: []Effect{StartTurn}},
	{From: Working, FromReason: turn, On: Finished, Arg: "done", To: Working, Reason: "publishing", Effects: []Effect{Publish}},
	{From: Working, FromReason: turn, On: Finished, Arg: "no_change", To: Closed, Reason: "no_change", Effects: []Effect{RemoveWorktree, WakeOrchestrator}},
	{From: Working, FromReason: turn, On: Finished, Arg: "blocked", To: Blocked, Reason: "question", Effects: []Effect{WakeOrchestrator}},
	{From: Working, FromReason: turn, On: Asked, To: Blocked, Reason: "question", Effects: []Effect{WakeOrchestrator}},

	// working, either way
	{From: Working, On: Message},
	{From: Working, On: Hold, Hold: +1},
	{From: Working, On: Release, Hold: -1},
	{From: Working, On: Kill, To: Closed, Reason: "killed", Effects: []Effect{StopTurn, RemoveWorktree, ClosePR}},
	{From: Working, On: PRObserved, Arg: "merged", To: Merged, Fix: -1, Effects: []Effect{StopTurn, RemoveWorktree, WakeOrchestrator}},
	{From: Working, On: PRObserved, Arg: "closed", To: Closed, Reason: "pr_closed", Effects: []Effect{StopTurn, RemoveWorktree, WakeOrchestrator}},

	// blocked
	{From: Blocked, FromReason: "question", On: Answered, To: Working, Reason: "message", Effects: []Effect{StartTurn}},
	{From: Blocked, On: Message, To: Working, Reason: "message", Effects: []Effect{StartTurn}},
	{From: Blocked, On: Retry, To: Ready, Reason: "slot", Fix: -1},
	{From: Blocked, On: Hold, Hold: +1},
	{From: Blocked, On: Release, Hold: -1},
	{From: Blocked, On: Kill, To: Closed, Reason: "killed", Effects: []Effect{RemoveWorktree, ClosePR}},
	{From: Blocked, On: PRObserved, Arg: "merged", To: Merged, Fix: -1, Effects: []Effect{RemoveWorktree, WakeOrchestrator}},
	{From: Blocked, On: PRObserved, Arg: "closed", To: Closed, Reason: "pr_closed", Effects: []Effect{RemoveWorktree, WakeOrchestrator}},
	{From: Blocked, On: TurnEnded},

	// pr_open
	{From: PROpen, On: PRObserved, Arg: "merged", To: Merged, Fix: -1, Effects: []Effect{RemoveWorktree, WakeOrchestrator}},
	{From: PROpen, On: PRObserved, Arg: "closed", To: Closed, Reason: "pr_closed", Effects: []Effect{RemoveWorktree, WakeOrchestrator}},
	{From: PROpen, On: PRObserved, Arg: "red", To: Working, Reason: "ci", Fix: +1, Effects: []Effect{StartTurn}},
	{From: PROpen, On: PRObserved, Arg: "conflicting", To: Working, Reason: "conflict", Fix: +1, Effects: []Effect{StartTurn}},
	{From: PROpen, On: PRObserved, Arg: "checks", To: PROpen, Reason: "checks"},
	{From: PROpen, On: PRObserved, Arg: "review|queue", To: PROpen, Reason: ArgReason, Fix: -1},
	{From: PROpen, On: Message, To: Working, Reason: "message", Effects: []Effect{StartTurn}},
	{From: PROpen, On: Hold, Hold: +1},
	{From: PROpen, On: Release, To: PROpen, Reason: "checks", Hold: -1},
	{From: PROpen, On: Kill, To: Closed, Reason: "killed", Effects: []Effect{RemoveWorktree, ClosePR}},
	{From: PROpen, On: TurnEnded},

	// merged: terminal. A late turn_ended is accepted and changes nothing.
	{From: Merged, On: TurnEnded},

	// closed
	{From: Closed, On: Retry, To: Ready, Reason: "slot", Fix: -1},
	{From: Closed, On: PRObserved, Arg: "merged", To: Merged, Fix: -1, Effects: []Effect{RemoveWorktree, WakeOrchestrator}},
	{From: Closed, On: TurnEnded},
}

// IllegalError says an event has no rule for the task's state.
type IllegalError struct {
	State  State
	Reason string
	Held   bool
	Event  Event
}

func (e *IllegalError) Error() string {
	at := string(e.State)
	if e.Reason != "" {
		at += ": " + e.Reason
	}
	if e.Held && e.Reason != "held" {
		at += " (held)"
	}
	return fmt.Sprintf("%s is not legal for a task that is %s", e.Event, at)
}

func (r Rule) matches(t Task, e Event) bool {
	if r.From != t.State || r.On != e.Kind || (r.Unheld && t.Held) || (r.Role != "" && r.Role != t.Role) {
		return false
	}
	if not, ok := strings.CutPrefix(r.FromReason, "!"); ok {
		if t.Reason == not {
			return false
		}
	} else if r.FromReason != "" && r.FromReason != t.Reason {
		return false
	}
	return r.Arg == "" || slices.Contains(strings.Split(r.Arg, "|"), e.Arg)
}

// checkEvent rejects an event no task could accept.
func checkEvent(e Event) error {
	if !slices.Contains(Kinds, e.Kind) {
		return fmt.Errorf("unknown event %q", e.Kind)
	}
	args, takes := Args[e.Kind]
	switch {
	case e.Kind == Waiting:
		if !ValidReason(Ready, e.Arg) {
			return fmt.Errorf("waiting(%s): not a reason a ready task can have", e.Arg)
		}
	case takes && !slices.Contains(args, e.Arg):
		return fmt.Errorf("%s: want one of %s", e, strings.Join(args, ", "))
	case !takes && e.Arg != "":
		return fmt.Errorf("%s takes no argument, got %q", e.Kind, e.Arg)
	}
	return nil
}

// Transition is the only way a task's state changes. It returns the task
// after the event and what the engine must do. An event that isn't legal
// returns the task unchanged and an *IllegalError.
func Transition(t Task, e Event) (Task, []Effect, error) {
	if err := checkEvent(e); err != nil {
		return t, nil, err
	}
	for _, r := range Table {
		if !r.matches(t, e) {
			continue
		}
		out, effects := t, slices.Clone(r.Effects)
		if r.To != "" {
			out.State, out.Reason = r.To, r.Reason
			if r.Reason == ArgReason {
				out.Reason = e.Arg
			}
		}
		limit := e.FixCap
		if limit <= 0 {
			limit = DefaultFixCap
		}
		switch {
		case r.Fix < 0:
			out.FixAttempts = 0
		case r.Fix > 0 && t.FixAttempts >= limit:
			out.State, out.Reason = Blocked, "gave_up"
			effects = []Effect{LabelNeedsHuman, NotifyOwner}
		case r.Fix > 0:
			out.FixAttempts++
		}
		if r.Hold != 0 {
			out.Held = r.Hold > 0
		}
		if e.Kind == Published {
			out.PR = e.PR
		}
		if out.Held && (out.State == Ready || out.State == PROpen) {
			out.Reason = "held"
		}
		return out, effects, nil
	}
	return t, nil, &IllegalError{State: t.State, Reason: t.Reason, Held: t.Held, Event: e}
}

// Markdown renders the states and the table. `saddle2 states` prints it and
// the glossary's state section is this text.
func Markdown() string {
	var b strings.Builder
	b.WriteString("| State | Meaning | Reasons |\n|---|---|---|\n")
	for _, s := range States {
		var reasons []string
		for _, r := range s.Reasons {
			if r == "" {
				continue
			}
			if strings.HasSuffix(r, ":") {
				r += "<task>"
			}
			reasons = append(reasons, "`"+r+"`")
		}
		meaning := s.Meaning
		if s.Terminal {
			meaning += "; terminal"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", s.State, meaning, strings.Join(reasons, ", "))
	}
	b.WriteString("\n| From | Event | To | Also |\n|---|---|---|---|\n")
	for _, r := range Table {
		from := string(r.From)
		if not, ok := strings.CutPrefix(r.FromReason, "!"); ok {
			from += ", not " + not
		} else if r.FromReason != "" {
			from += ": " + r.FromReason
		}
		ev := string(r.On)
		if r.Arg != "" {
			ev += "(" + strings.ReplaceAll(r.Arg, "|", ", ") + ")"
		}
		to := "no change"
		if r.To != "" {
			to = string(r.To)
			if r.Reason != "" {
				to += ": " + r.Reason
			}
		}
		var also []string
		if r.Role != "" {
			also = append(also, r.Role+" only")
		}
		if r.Unheld {
			also = append(also, "unless held")
		}
		also = append(also, map[int]string{-1: "fixes reset", 1: "counts a fix"}[r.Fix], map[int]string{-1: "held off", 1: "held on"}[r.Hold])
		for _, e := range r.Effects {
			also = append(also, string(e))
		}
		also = slices.DeleteFunc(also, func(s string) bool { return s == "" })
		fmt.Fprintf(&b, "| `%s` | `%s` | `%s` | %s |\n", from, ev, to, strings.Join(also, ", "))
	}
	b.WriteString("\nA rule that counts a fix on a task already at the cap goes to `blocked: gave_up` instead, " +
		"with label_needs_human and notify_owner. A held task that lands in `ready` or `pr_open` has the reason `held`. " +
		"An event with no row is refused and changes nothing.\n")
	return b.String()
}
