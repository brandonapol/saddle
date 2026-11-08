// Package soak is saddle's soak harness (#831): a scripted run of many
// changes with random after edges and real build dependencies, driven
// through the real app against fakegh and fake agents while faults are
// injected. Nobody acts during a run: autopilot spawns, the train lands,
// auto-merge merges and the CI watchers repair. The run passes when every
// change ends merged (or dropped with a blocker issue), no invariant
// violation outlasts its budget, repairs stay at or below a tenth of the
// changes and no repair targets a saddle-owned check.
//
// The scenario (which changes, which edges, which faults when) is a pure
// function of a seed, so a failing run is replayed by its seed. Run drives
// one scenario through an e2e World (soak_test.go, build tag e2e); the
// verdict logic is pure and tested on its own.
package soak

import (
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strings"
)

// FaultKind names one injected fault.
type FaultKind string

// The faults of #831. Per-change faults ride on one change; the others are
// windows or events in the run.
const (
	// RedCI: the change's CI goes red (a failing test the local gate
	// doesn't run), at whatever layer of its stack it sits.
	RedCI FaultKind = "red_ci"
	// Flaky: the change's CI fails once on a lost runner, which GitHub
	// passes on a rerun.
	Flaky FaultKind = "flaky"
	// KilledWorker: the change's agent hangs and its window is killed.
	KilledWorker FaultKind = "killed_worker"
	// RateLimit: GitHub answers every call with a rate limit for a window
	// of 30 simulated minutes.
	RateLimit FaultKind = "rate_limit"
	// AsyncMerge: native stacks are on, so GitHub refuses gh pr merge for a
	// stacked PR and merges it only through merge-async.
	AsyncMerge FaultKind = "async_merge"
	// RedBase: CI is red on main for a window, so every PR's CI is red too.
	RedBase FaultKind = "red_base"
	// EngineKill: a saddle land (the train's reconcile) is killed mid-run.
	EngineKill FaultKind = "engine_kill"
	// ExcludedAdapter: one change's ticket asks for an adapter the policy
	// excludes; the spawn must not use it.
	ExcludedAdapter FaultKind = "excluded_adapter"
)

// AllFaults lists every fault kind, in a fixed order.
var AllFaults = []FaultKind{RedCI, Flaky, KilledWorker, RateLimit, AsyncMerge, RedBase, EngineKill, ExcludedAdapter}

// perChange are the faults that ride on one change.
var perChange = []FaultKind{RedCI, Flaky, KilledWorker, ExcludedAdapter}

// TickMinutes is how much simulated time one harness tick stands for.
const TickMinutes = 2

// Config shapes a scenario.
type Config struct {
	Seed    int64
	Changes int
	// MaxAfter caps a change's after edges (default 2).
	MaxAfter int
	// EdgeRate is the chance a change has any after edge (default 0.4).
	EdgeRate float64
	// FaultRate is the chance a change carries a per-change fault, past the
	// one of each kind every scenario has (default 0.15).
	FaultRate float64
	// Skip drops fault kinds (the known-failing ones, each with its issue).
	Skip map[FaultKind]string
}

// Change is one unit of scripted work: issue #Issue, which writes package
// ID and imports the package of every change in After, so it won't build
// without them.
type Change struct {
	ID     string      `json:"id"`
	Issue  int         `json:"issue"`
	After  []string    `json:"after,omitempty"`
	Faults []FaultKind `json:"faults,omitempty"`
}

// Has reports whether the change carries fault k.
func (c Change) Has(k FaultKind) bool { return slices.Contains(c.Faults, k) }

// Event is a run-level fault: from tick At for Ticks ticks (one for an
// instant event).
type Event struct {
	Kind  FaultKind `json:"kind"`
	At    int       `json:"at"`
	Ticks int       `json:"ticks"`
}

// Active reports whether e covers tick.
func (e Event) Active(tick int) bool { return tick >= e.At && tick < e.At+e.Ticks }

// Scenario is one soak run's script.
type Scenario struct {
	Seed       int64    `json:"seed"`
	Changes    []Change `json:"changes"`
	Events     []Event  `json:"events,omitempty"`
	AsyncMerge bool     `json:"async_merge"`
}

// Change returns the change with id, or false.
func (s Scenario) Change(id string) (Change, bool) {
	i := slices.IndexFunc(s.Changes, func(c Change) bool { return c.ID == id })
	if i < 0 {
		return Change{}, false
	}
	return s.Changes[i], true
}

// ByIssue returns the change for issue n, or false.
func (s Scenario) ByIssue(n int) (Change, bool) {
	if n < 1 || n > len(s.Changes) {
		return Change{}, false
	}
	return s.Changes[n-1], true
}

// Count is how many changes and events carry fault k.
func (s Scenario) Count(k FaultKind) int {
	n := 0
	for _, c := range s.Changes {
		if c.Has(k) {
			n++
		}
	}
	for _, e := range s.Events {
		if e.Kind == k {
			n++
		}
	}
	if k == AsyncMerge && s.AsyncMerge {
		n++
	}
	return n
}

// ChangeID is change i's id (from 1): c01, c02, ...
func ChangeID(i int) string { return fmt.Sprintf("c%02d", i) }

// Generate builds the scenario for c. It is a pure function of c: the same
// config always gives the same scenario.
func Generate(c Config) Scenario {
	if c.MaxAfter == 0 {
		c.MaxAfter = 2
	}
	if c.EdgeRate == 0 {
		c.EdgeRate = 0.4
	}
	if c.FaultRate == 0 {
		c.FaultRate = 0.15
	}
	r := rand.New(rand.NewSource(c.Seed))
	skip := func(k FaultKind) bool { _, ok := c.Skip[k]; return ok }
	sc := Scenario{Seed: c.Seed}
	for i := 1; i <= c.Changes; i++ {
		ch := Change{ID: ChangeID(i), Issue: i}
		if i > 1 && r.Float64() < c.EdgeRate {
			n := 1 + r.Intn(c.MaxAfter)
			for _, j := range r.Perm(i - 1)[:min(n, i-1)] {
				ch.After = append(ch.After, ChangeID(j+1))
			}
			sort.Strings(ch.After)
		}
		sc.Changes = append(sc.Changes, ch)
	}

	// One of each per-change fault on distinct changes, then more at
	// FaultRate. A change carries at most one fault, so each failure has
	// one cause.
	var kinds []FaultKind
	for _, k := range perChange {
		if !skip(k) {
			kinds = append(kinds, k)
		}
	}
	free := r.Perm(len(sc.Changes))
	for _, k := range kinds {
		if len(free) == 0 {
			break
		}
		sc.Changes[free[0]].Faults = []FaultKind{k}
		free = free[1:]
	}
	// Red CI needs a repair, and repairs are capped at a tenth of the
	// changes: half that is injected, so flaky checks have room to be
	// mistaken for red without failing the cap.
	redCap := max(1, c.Changes/20)
	for _, i := range free {
		if len(kinds) == 0 || r.Float64() >= c.FaultRate {
			continue
		}
		k := kinds[r.Intn(len(kinds))]
		if k == ExcludedAdapter || (k == RedCI && sc.Count(RedCI) >= redCap) {
			continue // one excluded-adapter ticket is enough
		}
		sc.Changes[i].Faults = []FaultKind{k}
	}

	// Run-level faults land while work is in flight: the rate limit and the
	// red base early, the engine kill wherever the train is busy.
	span := max(4, c.Changes/2)
	if !skip(RateLimit) {
		sc.Events = append(sc.Events, Event{Kind: RateLimit, At: 2 + r.Intn(span), Ticks: 30 / TickMinutes})
	}
	if !skip(RedBase) {
		sc.Events = append(sc.Events, Event{Kind: RedBase, At: 2 + r.Intn(span), Ticks: 2 + r.Intn(3)})
	}
	if !skip(EngineKill) {
		for range max(1, c.Changes/20) {
			sc.Events = append(sc.Events, Event{Kind: EngineKill, At: 1 + r.Intn(span), Ticks: 1})
		}
	}
	sort.SliceStable(sc.Events, func(i, j int) bool {
		if sc.Events[i].At != sc.Events[j].At {
			return sc.Events[i].At < sc.Events[j].At
		}
		return sc.Events[i].Kind < sc.Events[j].Kind
	})
	sc.AsyncMerge = !skip(AsyncMerge)
	return sc
}

// Active lists the run-level faults covering tick.
func (s Scenario) Active(tick int) []FaultKind {
	var out []FaultKind
	for _, e := range s.Events {
		if e.Active(tick) && !slices.Contains(out, e.Kind) {
			out = append(out, e.Kind)
		}
	}
	return out
}

// String is a one-line summary for logs.
func (s Scenario) String() string {
	edges := 0
	for _, c := range s.Changes {
		edges += len(c.After)
	}
	var fs []string
	for _, k := range AllFaults {
		if n := s.Count(k); n > 0 {
			fs = append(fs, fmt.Sprintf("%s=%d", k, n))
		}
	}
	return fmt.Sprintf("seed %d: %d changes, %d after edges, faults %s", s.Seed, len(s.Changes), edges, strings.Join(fs, " "))
}
