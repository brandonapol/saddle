package soak

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/brandonapol/saddle/internal/invariants"
)

// Budgets is how many ticks a violation of each invariant may last before
// the soak fails. A kind missing here gets DefaultBudget.
type Budgets map[invariants.Kind]int

// DefaultBudget is a violation's budget when Budgets doesn't name its kind:
// ten ticks, twenty simulated minutes, past which the run fails.
const DefaultBudget = 10

// DefaultBudgets are the soak's budgets. I7 (a repair of a saddle-owned
// check) and I8 (a spawn past the policy) have none: one tick fails.
var DefaultBudgets = Budgets{
	invariants.SelfRepair:   0,
	invariants.PolicyBypass: 0,
}

func (b Budgets) of(k invariants.Kind) int {
	if n, ok := b[k]; ok {
		return n
	}
	return DefaultBudget
}

// Span is one violation's life: the ticks it was seen, From to To.
type Span struct {
	Key    string          `json:"key"`
	Kind   invariants.Kind `json:"kind"`
	From   int             `json:"from"`
	To     int             `json:"to"`
	Open   bool            `json:"open"` // still present at the last tick
	Detail string          `json:"detail"`
}

// Ticks is how many ticks the span lasted.
func (s Span) Ticks() int { return s.To - s.From + 1 }

// Ledger records every invariant violation's spans across a run.
type Ledger struct {
	open  map[string]*Span
	Spans []Span
	last  int
}

// Observe records the violations seen at tick. One missing at tick closes.
func (l *Ledger) Observe(tick int, vs []invariants.Violation) {
	if l.open == nil {
		l.open = map[string]*Span{}
	}
	l.last = tick
	seen := map[string]bool{}
	for _, v := range vs {
		k := v.Key()
		seen[k] = true
		if s, ok := l.open[k]; ok {
			s.To, s.Detail = tick, v.Detail
			continue
		}
		l.open[k] = &Span{Key: k, Kind: v.Kind, From: tick, To: tick, Detail: v.Detail}
	}
	for k, s := range l.open {
		if !seen[k] {
			l.Spans = append(l.Spans, *s)
			delete(l.open, k)
		}
	}
}

// All is every span, the ones still open marked so, sorted by start.
func (l *Ledger) All() []Span {
	out := append([]Span(nil), l.Spans...)
	for _, s := range l.open {
		c := *s
		c.Open = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Over lists the spans that outlasted their budget: longer than it, or
// still open at the end (nothing healed them).
func (l *Ledger) Over(b Budgets) []Span {
	var out []Span
	for _, s := range l.All() {
		if s.Ticks() > b.of(s.Kind) || s.Open {
			out = append(out, s)
		}
	}
	return out
}

// Terminal states of a change.
const (
	Merged  = "merged"  // its work is on main through a merged PR
	Dropped = "dropped" // given up, with a blocker issue that says why
	Stuck   = "stuck"   // neither: the run failed for it
)

// Outcome is where one change ended.
type Outcome struct {
	Change string `json:"change"`
	Task   string `json:"task,omitempty"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// Repair is one repair task saddle spawned and the checks it went after.
type Repair struct {
	Task   string   `json:"task"`
	For    string   `json:"for"`
	Checks []string `json:"checks,omitempty"`
}

// Result is what a run did.
type Result struct {
	Scenario Scenario  `json:"scenario"`
	Ticks    int       `json:"ticks"`
	Outcomes []Outcome `json:"outcomes"`
	Ledger   *Ledger   `json:"-"`
	Repairs  []Repair  `json:"repairs,omitempty"`
	// Actions are human or orchestrator actions taken during the run: any
	// is a failure (the harness itself never takes one; a card answered or
	// a notice that demands a human is counted here).
	Actions []string `json:"actions,omitempty"`
	// Excluded are tasks spawned on an adapter the policy excludes.
	Excluded []string `json:"excluded,omitempty"`
}

// MaxRepairs is the repair cap for n changes: a tenth, rounded down.
func MaxRepairs(n int) int { return n / 10 }

// Failures lists every way r falls short of #831's bar, or none.
func (r Result) Failures(b Budgets) []string {
	var out []string
	for _, o := range r.Outcomes {
		if o.State != Merged && o.State != Dropped {
			out = append(out, fmt.Sprintf("%s (%s) never reached a terminal state: %s", o.Change, o.Task, o.Detail))
		}
	}
	if missing := len(r.Scenario.Changes) - len(r.Outcomes); missing > 0 {
		out = append(out, fmt.Sprintf("%d changes have no outcome", missing))
	}
	for _, t := range r.Excluded {
		out = append(out, "spawned on an excluded adapter: "+t)
	}
	for _, a := range r.Actions {
		out = append(out, "human or orchestrator action: "+a)
	}
	if r.Ledger != nil {
		for _, s := range r.Ledger.Over(b) {
			state := "cleared"
			if s.Open {
				state = "still open at the end"
			}
			out = append(out, fmt.Sprintf("invariant %s lasted %d ticks (budget %d, %s): %s", s.Key, s.Ticks(), b.of(s.Kind), state, s.Detail))
		}
	}
	if n, limit := len(r.Repairs), MaxRepairs(len(r.Scenario.Changes)); n > limit {
		var ids []string
		for _, rp := range r.Repairs {
			ids = append(ids, rp.Task+" for "+rp.For)
		}
		out = append(out, fmt.Sprintf("%d repairs for %d changes, over the cap of %d: %s", n, len(r.Scenario.Changes), limit, strings.Join(ids, ", ")))
	}
	for _, rp := range r.Repairs {
		if i := slices.IndexFunc(rp.Checks, invariants.SaddleCheck); i >= 0 {
			out = append(out, fmt.Sprintf("repair %s for %s targets saddle-owned check %s", rp.Task, rp.For, rp.Checks[i]))
		}
	}
	return out
}
