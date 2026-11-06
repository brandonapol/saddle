package prgraph

import "time"

// StateRule is one row of ADR 0009 §2's table: a state, its default
// deadline and its automatic exits. Every exit is an action Reconcile emits,
// never a command a human runs.
type StateRule struct {
	State ChangeState
	// Deadline is the default clock; zero for terminal states.
	Deadline func(Policy) time.Duration
	// Exits are the states Reconcile may move the change to, each with the
	// action that does it.
	Exits []StateExit
}

// StateExit is one automatic exit.
type StateExit struct {
	To ChangeState
	By ActionKind // the action that takes the change there
	On string     // what triggers it
}

// StateTable is the change state machine.
func StateTable() []StateRule {
	return []StateRule{
		{Queued, func(p Policy) time.Duration { return p.withDefaults().QueuedDeadline }, []StateExit{
			{Landed, ActTransition, "gate green on base + deps"},
			{Red, ActTransition, "gate red, conflict, or deadline"},
		}},
		{Landed, func(p Policy) time.Duration { return p.withDefaults().LandedDeadline }, []StateExit{
			{Published, ActTransition, "PR open on its desired base (after Publish or Move)"},
			{Red, ActTransition, "deadline: red(publish)"},
		}},
		{Published, func(p Policy) time.Duration { return p.withDefaults().PublishedDeadline }, []StateExit{
			{Green, ActTransition, "required checks green"},
			{Red, ActTransition, "a required check red, or deadline: red(checks_stuck)"},
		}},
		{Green, func(p Policy) time.Duration { return p.withDefaults().GreenDeadline }, []StateExit{
			{Merged, ActMerge, "deps merged, automerge on"},
			{Red, ActTransition, "deadline: red(merge), or a check turned red"},
			{Green, ActEscalate, "automerge off, deadline: digest line and a new deadline"},
		}},
		{Red, func(p Policy) time.Duration { return p.withDefaults().FixWait }, []StateExit{
			{Queued, ActTransition, "new head"},
			{Published, ActTransition, "the failing check is no longer red"},
			{Superseded, ActTransition, "repair cap reached: superseded(gave_up)"},
		}},
		{Merged, nil, nil},
		{Superseded, nil, []StateExit{
			{Merged, ActTransition, "base contains the work"},
		}},
	}
}
