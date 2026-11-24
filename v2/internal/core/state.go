package core

import "strings"

// State is where a task is. A task is in exactly one, with a reason.
type State string

// The six states. There is no seventh: a new case is a reason on one of these.
const (
	Ready   State = "ready"
	Working State = "working"
	Blocked State = "blocked"
	PROpen  State = "pr_open"
	Merged  State = "merged"
	Closed  State = "closed"
)

// StateInfo describes one state for people and for the tests.
type StateInfo struct {
	State   State
	Meaning string
	// Reasons are the reasons a task may carry in this state. One that ends
	// in a colon takes a task name after it: "after:" allows "after:t3".
	Reasons []string
	// Terminal states start no work. Only closed can be left, by retry.
	Terminal bool
}

// States lists every state in the order a task usually passes through them.
var States = []StateInfo{
	{Ready, "waiting to start", []string{"slot", "after:", "overlap:", "no_provider", "held", "wake"}, false},
	{Working, "a turn is running or about to, or saddle is publishing its result",
		[]string{"new", "message", "unfinished", "publishing", "conflict", "ci", "check", "rules"}, false},
	{Blocked, "waiting on an answer", []string{"question", "gave_up", "turn_failed"}, false},
	{PROpen, "published, not merged", []string{"checks", "review", "queue", "held"}, false},
	{Merged, "on base", []string{""}, true},
	{Closed, "ended without merging", []string{"killed", "no_change", "pr_closed"}, true},
}

// Info returns the description of s, and false if s is not a state.
func Info(s State) (StateInfo, bool) {
	for _, i := range States {
		if i.State == s {
			return i, true
		}
	}
	return StateInfo{}, false
}

// ValidReason reports whether a task in state s may carry reason.
func ValidReason(s State, reason string) bool {
	info, ok := Info(s)
	if !ok {
		return false
	}
	for _, r := range info.Reasons {
		if r == reason {
			return !strings.HasSuffix(r, ":")
		}
		if strings.HasSuffix(r, ":") && strings.HasPrefix(reason, r) && len(reason) > len(r) {
			return true
		}
	}
	return false
}

// Task is the part of a task the state machine reads and writes. The store
// keeps the rest.
type Task struct {
	Role        string // "worker" or "orchestrator"
	State       State
	Reason      string
	FixAttempts int   // fixes since the PR was last green
	Held        bool  // the owner said not to start or merge it
	PR          int64 // 0 until published
}

// Orchestrator is the role of the one task the owner talks to.
const Orchestrator = "orchestrator"

// New is a task before anything has happened to it.
func New(role string) Task {
	if role == Orchestrator {
		return Task{Role: role, State: Ready, Reason: "wake"}
	}
	return Task{Role: "worker", State: Ready, Reason: "slot"}
}
