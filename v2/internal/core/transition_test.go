package core

import (
	"errors"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

// legal says, for every state and every kind of event, whether some task in
// that state accepts some event of that kind. It is written out by hand so
// that a change to the table has to be made twice.
var legal = map[State]map[Kind]bool{
	Ready: {SlotGranted: true, Waiting: true, TurnEnded: true, Message: true, PRObserved: true,
		Kill: true, Hold: true, Release: true},
	Working: {TurnEnded: true, Finished: true, Asked: true, Message: true, Published: true,
		PublishFailed: true, PRObserved: true, Kill: true, Hold: true, Release: true},
	Blocked: {TurnEnded: true, Answered: true, Message: true, PRObserved: true, Kill: true,
		Retry: true, Hold: true, Release: true},
	PROpen: {TurnEnded: true, Message: true, PRObserved: true, Kill: true, Hold: true, Release: true},
	Merged: {TurnEnded: true},
	Closed: {TurnEnded: true, PRObserved: true, Retry: true},
}

// reasons expands a state's reasons, giving "after:" a task to name.
func reasons(s StateInfo) []string {
	var out []string
	for _, r := range s.Reasons {
		if strings.HasSuffix(r, ":") {
			r += "t1"
		}
		out = append(out, r)
	}
	return out
}

// events is every event of a kind: one per argument it takes.
func events(k Kind) []Event {
	args := Args[k]
	if k == Waiting {
		args = reasons(States[0])
	}
	if len(args) == 0 {
		return []Event{{Kind: k, PR: 7}}
	}
	var out []Event
	for _, a := range args {
		out = append(out, Event{Kind: k, Arg: a})
	}
	return out
}

// tasks is every task shape worth trying in a state.
func tasks(s StateInfo) []Task {
	var out []Task
	for _, role := range []string{"worker", Orchestrator} {
		for _, r := range reasons(s) {
			out = append(out, Task{Role: role, State: s.State, Reason: r, Held: r == "held"})
		}
	}
	return out
}

func TestEveryStateAndEventPair(t *testing.T) {
	for _, s := range States {
		for _, k := range Kinds {
			accepted := false
			for _, task := range tasks(s) {
				for _, e := range events(k) {
					got, effects, err := Transition(task, e)
					if err == nil {
						accepted = true
						continue
					}
					var illegal *IllegalError
					if !errors.As(err, &illegal) {
						t.Errorf("%+v on %s: error %v is not an *IllegalError", task, e, err)
					} else if !strings.Contains(err.Error(), string(s.State)) || !strings.Contains(err.Error(), string(k)) {
						t.Errorf("error %q does not name both %s and %s", err, s.State, k)
					}
					if got != task || effects != nil {
						t.Errorf("%+v refused %s but returned %+v %v", task, e, got, effects)
					}
				}
			}
			if accepted != legal[s.State][k] {
				t.Errorf("%s in %s: legal = %v, want %v", k, s.State, accepted, legal[s.State][k])
			}
		}
	}
}

func TestTableIsConsistent(t *testing.T) {
	reached := map[State][]string{Ready: {"slot", "wake", "held"}, PROpen: {"held"}, Blocked: {"gave_up"}}
	for i, r := range Table {
		if _, ok := Info(r.From); !ok || !slices.Contains(Kinds, r.On) {
			t.Errorf("rule %d: unknown state %q or event %q", i, r.From, r.On)
		}
		if fr := strings.TrimPrefix(r.FromReason, "!"); fr != "" && !ValidReason(r.From, fr) {
			t.Errorf("rule %d: %s has no reason %q", i, r.From, fr)
		}
		for _, a := range strings.Split(r.Arg, "|") {
			if a != "" && !slices.Contains(Args[r.On], a) {
				t.Errorf("rule %d: %s takes no argument %q", i, r.On, a)
			}
		}
		if r.To == "" {
			if r.Reason != "" || r.Fix != 0 {
				t.Errorf("rule %d: changes nothing but sets a reason or the fix count", i)
			}
			continue
		}
		targets := []string{r.Reason}
		if r.Reason == ArgReason {
			if targets = strings.Split(r.Arg, "|"); r.Arg == "" {
				targets = Args[r.On]
			}
			if r.On == Waiting {
				targets = reasons(States[0])
			}
		}
		for _, reason := range targets {
			if !ValidReason(r.To, reason) {
				t.Errorf("rule %d: %s has no reason %q", i, r.To, reason)
			}
			reached[r.To] = append(reached[r.To], reason)
		}
	}
	for _, s := range States {
		for _, r := range reasons(s) {
			if !slices.Contains(reached[s.State], r) {
				t.Errorf("nothing puts a task in %s: %s", s.State, r)
			}
		}
	}
	if !ValidReason(Ready, "after:t3") || ValidReason(Ready, "after:") || ValidReason(Ready, "fixing") || ValidReason("idle", "") {
		t.Error("ValidReason accepts or refuses the wrong reasons")
	}
}

// No latch: a task that isn't finished can always be moved by something the
// owner can type. A new reason with no way out fails here.
func TestNoLatch(t *testing.T) {
	for _, s := range States {
		if s.Terminal {
			continue
		}
		for _, task := range tasks(s) {
			var exits []string
			for _, k := range Commands {
				got, _, err := Transition(task, Event{Kind: k})
				if err == nil && (got.State != task.State || got.Reason != task.Reason) {
					exits = append(exits, string(k))
				}
			}
			if len(exits) == 0 {
				t.Errorf("%s %s: %s has no exit by any of %v", task.Role, task.State, task.Reason, Commands)
			}
		}
	}
	// The specific exits the design promises.
	for _, c := range []struct {
		from Task
		by   Kind
		to   State
	}{
		{Task{State: Ready, Reason: "held", Held: true}, Release, Ready},
		{Task{State: PROpen, Reason: "held", Held: true}, Release, PROpen},
		{Task{State: Blocked, Reason: "question"}, Answered, Working},
		{Task{State: Blocked, Reason: "gave_up", FixAttempts: 2}, Retry, Ready},
		{Task{State: Blocked, Reason: "turn_failed"}, Retry, Ready},
		{Task{State: Closed, Reason: "killed"}, Retry, Ready},
	} {
		got, _, err := Transition(c.from, Event{Kind: c.by})
		if err != nil || got.State != c.to || got.Reason == c.from.Reason || got.FixAttempts != 0 {
			t.Errorf("%s on %+v = %+v, %v; want %s, a new reason and no fixes counted", c.by, c.from, got, err, c.to)
		}
	}
}

func TestAnySequenceEndsInAValidState(t *testing.T) {
	rng := rand.New(rand.NewSource(959))
	var all []Event
	for _, k := range Kinds {
		all = append(all, events(k)...)
	}
	all = append(all, Event{Kind: "landed"}, Event{Kind: TurnEnded, Arg: "parked"}, Event{Kind: Kill, Arg: "x"})
	for _, s := range States {
		for _, start := range tasks(s) {
			for range 200 {
				task := start
				for range 40 {
					e := all[rng.Intn(len(all))]
					e.FixCap = rng.Intn(4)
					got, _, err := Transition(task, e)
					switch {
					case err != nil && got != task:
						t.Fatalf("%s on %+v failed (%v) but changed the task to %+v", e, task, err, got)
					case !ValidReason(got.State, got.Reason):
						t.Fatalf("%s on %+v gave %s: %s, which is not a state and reason", e, task, got.State, got.Reason)
					case task.State == Merged && got != task:
						t.Fatalf("%s moved a merged task to %+v", e, got)
					case got.FixAttempts < 0 || got.FixAttempts > max(task.FixAttempts, 3):
						t.Fatalf("%s on %+v counted %d fixes", e, task, got.FixAttempts)
					case got.Held && (got.State == Ready || got.State == PROpen) && got.Reason != "held":
						t.Fatalf("%s on %+v left a held task %s: %s", e, task, got.State, got.Reason)
					}
					task = got
				}
			}
		}
	}
}

// step applies events in order and fails the test on the first refusal.
func step(t *testing.T, task Task, evs ...Event) (Task, []Effect) {
	t.Helper()
	var effects []Effect
	for _, e := range evs {
		var err error
		if task, effects, err = Transition(task, e); err != nil {
			t.Fatalf("%s: %v", e, err)
		}
	}
	return task, effects
}

func TestFromTicketToMerged(t *testing.T) {
	task, effects := step(t, New("worker"), Event{Kind: Waiting, Arg: "after:t3"}, Event{Kind: SlotGranted})
	if task.State != Working || task.Reason != "new" || !slices.Equal(effects, []Effect{StartTurn}) {
		t.Fatalf("after a slot: %+v %v", task, effects)
	}
	// finish, then the process exits: the late turn_ended changes nothing.
	task, _ = step(t, task, Event{Kind: Finished, Arg: "done"}, Event{Kind: TurnEnded, Arg: "ok"})
	if task.Reason != "publishing" {
		t.Fatalf("after finish(done): %+v, want working: publishing", task)
	}
	if _, _, err := Transition(task, Event{Kind: Finished, Arg: "done"}); err == nil {
		t.Error("a task that is publishing accepted a second finish")
	}
	task, _ = step(t, task, Event{Kind: Published, PR: 41})
	if task.State != PROpen || task.Reason != "checks" || task.PR != 41 {
		t.Fatalf("after publishing: %+v", task)
	}
	task, effects = step(t, task, Event{Kind: PRObserved, Arg: "queue"}, Event{Kind: PRObserved, Arg: "merged"})
	if task.State != Merged || task.Reason != "" || !slices.Contains(effects, RemoveWorktree) {
		t.Fatalf("after the merge: %+v %v", task, effects)
	}
}

func TestFixesAreCountedCappedAndReset(t *testing.T) {
	open := Task{Role: "worker", State: PROpen, Reason: "checks", PR: 9}
	red := Event{Kind: PRObserved, Arg: "red"}
	back := []Event{{Kind: Finished, Arg: "done"}, {Kind: Published, PR: 9}}

	task, effects := step(t, open, red)
	if task.State != Working || task.Reason != "ci" || task.FixAttempts != 1 || !slices.Equal(effects, []Effect{StartTurn}) {
		t.Fatalf("first red: %+v %v", task, effects)
	}
	task, _ = step(t, task, back...)
	task, _ = step(t, task, Event{Kind: PRObserved, Arg: "conflicting"})
	if task.Reason != "conflict" || task.FixAttempts != 2 {
		t.Fatalf("second fix: %+v", task)
	}
	task, _ = step(t, task, back...)
	task, effects = step(t, task, red)
	if task.State != Blocked || task.Reason != "gave_up" || !slices.Equal(effects, []Effect{LabelNeedsHuman, NotifyOwner}) {
		t.Fatalf("third red: %+v %v, want blocked: gave_up and the owner told", task, effects)
	}
	// A human fixes and merges it while saddle has given up.
	if merged, _ := step(t, task, Event{Kind: PRObserved, Arg: "merged"}); merged.State != Merged {
		t.Errorf("a PR merged by hand left the task %+v", merged)
	}

	// Green resets the count; so does a larger cap on the event.
	green, _ := step(t, Task{State: PROpen, Reason: "checks", FixAttempts: 2}, Event{Kind: PRObserved, Arg: "review"})
	if green.FixAttempts != 0 || green.Reason != "review" {
		t.Errorf("green: %+v, want no fixes counted", green)
	}
	more, _ := step(t, Task{State: PROpen, Reason: "checks", FixAttempts: 2}, Event{Kind: PRObserved, Arg: "red", FixCap: 5})
	if more.State != Working || more.FixAttempts != 3 {
		t.Errorf("cap 5: %+v, want a third fix", more)
	}
	// A failed publish counts too, so a worker can't loop on a red check.
	failed, _ := step(t, Task{State: Working, Reason: "publishing", FixAttempts: 2}, Event{Kind: PublishFailed, Arg: "check"})
	if failed.State != Blocked {
		t.Errorf("publish_failed at the cap: %+v", failed)
	}
}

func TestATurnThatEndsWithoutFinishGetsOneMoreTurn(t *testing.T) {
	task, effects := step(t, Task{Role: "worker", State: Working, Reason: "new"}, Event{Kind: TurnEnded, Arg: "crash"})
	if task.Reason != "unfinished" || !slices.Equal(effects, []Effect{StartTurn}) {
		t.Fatalf("first: %+v %v", task, effects)
	}
	task, _ = step(t, task, Event{Kind: TurnEnded, Arg: "ok"})
	if task.State != Blocked || task.Reason != "turn_failed" {
		t.Fatalf("second: %+v, want blocked: turn_failed", task)
	}
	quota, _ := step(t, Task{State: Working, Reason: "ci"}, Event{Kind: TurnEnded, Arg: "quota"})
	if quota.State != Ready || quota.Reason != "no_provider" {
		t.Errorf("quota: %+v, want ready: no_provider", quota)
	}
	// The orchestrator's turn ending is not a failure: it waits to be woken.
	orch, _ := step(t, New(Orchestrator), Event{Kind: SlotGranted}, Event{Kind: TurnEnded, Arg: "ok"})
	if orch.State != Ready || orch.Reason != "wake" {
		t.Errorf("orchestrator: %+v, want ready: wake", orch)
	}
}

func TestHeld(t *testing.T) {
	task, _ := step(t, New("worker"), Event{Kind: Hold})
	if !task.Held || task.Reason != "held" {
		t.Fatalf("hold: %+v", task)
	}
	if _, _, err := Transition(task, Event{Kind: SlotGranted}); err == nil {
		t.Error("a held task was given a slot")
	}
	if kept, _ := step(t, task, Event{Kind: Waiting, Arg: "slot"}); kept.Reason != "held" {
		t.Errorf("the scheduler overwrote held with %q", kept.Reason)
	}
	// Held while working: it publishes, and then waits as pr_open: held.
	w, _ := step(t, Task{State: Working, Reason: "publishing"}, Event{Kind: Hold}, Event{Kind: Published, PR: 3})
	if w.State != PROpen || w.Reason != "held" {
		t.Errorf("published while held: %+v", w)
	}
	if w, _ = step(t, w, Event{Kind: PRObserved, Arg: "queue"}, Event{Kind: Release}); w.Held || w.Reason != "checks" {
		t.Errorf("released: %+v", w)
	}
}

func TestMarkdownListsEveryStateAndRule(t *testing.T) {
	md := Markdown()
	for _, s := range States {
		if !strings.Contains(md, "| `"+string(s.State)+"` |") {
			t.Errorf("no row for %s", s.State)
		}
	}
	if got := strings.Count(md, "\n| `"); got != len(States)+len(Table) {
		t.Errorf("%d rows, want %d states and %d rules", got, len(States), len(Table))
	}
	for _, want := range []string{"`after:<task>`", "`turn_ended(ok, crash, timeout)`", "`working, not publishing`", "unless held", "counts a fix"} {
		if !strings.Contains(md, want) {
			t.Errorf("Markdown() lacks %s", want)
		}
	}
}
