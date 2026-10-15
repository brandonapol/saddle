package bench

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/store"
)

// Log is everything one run recorded. Score reads only this, so a saved
// log can be rescored.
type Log struct {
	Scenario    string    `json:"scenario"`
	Adapter     string    `json:"adapter"`
	Granularity string    `json:"granularity"`
	Started     time.Time `json:"started"`
	Ended       time.Time `json:"ended"`
	Expected    Expected  `json:"expected"`
	Plan        Plan      `json:"plan"`
	// Tasks are the tickets that were spawned and how they ended.
	Tasks []TaskRecord `json:"tasks"`
	// Events are saddle's events from the run, plus the runner's own
	// bench_* events, in id order.
	Events []Event `json:"events"`
	// Waits are autopilot's reasons for holding a ticket back, one per tick.
	Waits             []Wait `json:"waits,omitempty"`
	IntegrationGreen  bool   `json:"integration_green"`
	IntegrationOutput string `json:"integration_output,omitempty"`
	// EndedEarly says why the run stopped before autopilot finished.
	EndedEarly string `json:"ended_early,omitempty"`
	Tokens     *int   `json:"tokens,omitempty"`
}

// Expected is what scenario.toml says should happen.
type Expected struct {
	Overlap         string `json:"overlap"`
	SafeParallelism int    `json:"safe_parallelism"`
}

// TaskRecord is a spawned ticket's task and its final state.
type TaskRecord struct {
	Ticket int    `json:"ticket"`
	Task   string `json:"task"`
	Status string `json:"status"`
	Train  string `json:"train,omitempty"`
}

// Event is one store event.
type Event struct {
	ID   int64     `json:"id"`
	TS   time.Time `json:"ts"`
	Task string    `json:"task,omitempty"`
	Kind string    `json:"kind"`
	Data string    `json:"data,omitempty"`
}

// Wait is autopilot holding issue back for Why.
type Wait struct {
	Issue int    `json:"issue"`
	Why   string `json:"why"`
}

// Runner event kinds.
const (
	// EventStale is an edit whose anchor was in no file at spawn time;
	// Data is the edit's path.
	EventStale = "bench_stale"
)

// Report is a scored run. See docs/bench.md for each metric.
type Report struct {
	Scenario         string        `json:"scenario"`
	Adapter          string        `json:"adapter"`
	Granularity      string        `json:"granularity"`
	WallSeconds      float64       `json:"wall_seconds"`
	Tokens           *int          `json:"tokens"`
	Tickets          int           `json:"tickets"`
	Landed           int           `json:"landed"`
	AllLanded        bool          `json:"all_landed"`
	IntegrationGreen bool          `json:"integration_green"`
	DeniedWrites     int           `json:"denied_writes"`
	ClaimWaits       int           `json:"claim_waits"`
	TrainBounces     int           `json:"train_bounces"`
	TrainConflicts   int           `json:"train_conflicts"`
	TestFailures     int           `json:"test_failures"`
	Restacks         int           `json:"restacks"`
	RestackConflicts int           `json:"restack_conflicts"`
	StaleEdits       int           `json:"stale_edits"`
	NeededHelp       []string      `json:"needed_help"`
	PeakParallelism  int           `json:"peak_parallelism"`
	SafeParallelism  int           `json:"safe_parallelism"`
	Forecast         ForecastScore `json:"forecast"`
	EndedEarly       string        `json:"ended_early,omitempty"`
}

// ForecastScore compares the forecaster's predicted collisions with the
// ones that happened.
type ForecastScore struct {
	Expected       string `json:"expected"`  // scenario.toml's overlap
	Predicted      string `json:"predicted"` // the worst predicted pair's level
	PredictedPairs []Pair `json:"predicted_pairs"`
	ActualPairs    []Pair `json:"actual_pairs"`
	Hits           []Pair `json:"hits"`
	Misses         []Pair `json:"misses"` // collided, not predicted
	// Unseen were predicted but never collided: averted (often by
	// serializing them) or a false alarm.
	Unseen []Pair `json:"unseen"`
}

// predictAt is the lowest forecast level that counts as a predicted collision.
var predictAt = map[string]bool{"medium": true, "high": true, "critical": true}

// Score turns a run's log into its report. It is a pure function of l.
func Score(l Log) Report {
	r := Report{
		Scenario: l.Scenario, Adapter: l.Adapter, Granularity: l.Granularity,
		WallSeconds: l.Ended.Sub(l.Started).Seconds(), Tokens: l.Tokens,
		Tickets: len(l.Plan.Tickets), IntegrationGreen: l.IntegrationGreen,
		SafeParallelism: l.Expected.SafeParallelism, EndedEarly: l.EndedEarly,
	}
	ticketOf := map[string]int{} // task -> ticket
	landed := map[int]bool{}
	for _, t := range l.Tasks {
		ticketOf[t.Task] = t.Ticket
		if t.Status == store.Landed {
			landed[t.Ticket] = true
		}
	}
	r.Landed = len(landed)
	r.AllLanded = r.Landed == r.Tickets && r.Tickets > 0

	waited := map[int]bool{}
	for _, w := range l.Waits {
		if strings.Contains(w.Why, " overlaps ") || strings.Contains(w.Why, "declares no claims") {
			waited[w.Issue] = true
		}
	}
	r.ClaimWaits = len(waited)

	help := map[int]bool{}
	for _, t := range l.Plan.Tickets {
		if !landed[t.Number] {
			help[t.Number] = true
		}
	}
	actual := map[Pair]bool{}
	landedAt := map[int]int64{} // ticket -> landed event id
	live, peak := map[string]bool{}, 0
	for _, e := range l.Events {
		me := ticketOf[e.Task]
		switch e.Kind {
		case "spawn":
			live[e.Task] = true
			peak = max(peak, len(live))
		case "landed":
			delete(live, e.Task)
			landedAt[me] = e.ID
		case "deny":
			r.DeniedWrites++
			pairWith(actual, l.Plan, me, func(o PlannedTicket) bool { return claimsCover(o.Claims, e.Data) })
		case "train_" + store.TrainError:
			r.TrainConflicts++
			pairOnFiles(actual, l.Plan, me, strings.Split(e.Data, ", "))
		case "train_" + store.TestFailed:
			r.TestFailures++
			pkgs := packagesOf(l.Plan, me)
			pairWith(actual, l.Plan, me, func(o PlannedTicket) bool {
				at, ok := landedAt[o.Number]
				return ok && at < e.ID && overlaps(pkgs, dirsOf(o.Files))
			})
		case "restack":
			r.Restacks++
		case "auto_rebase":
			r.Restacks++
			if c := strings.TrimPrefix(e.Data, "conflicts="); c != "" {
				pairOnFiles(actual, l.Plan, me, strings.Split(c, ","))
			}
		case "restack_conflict":
			r.RestackConflicts++
			if _, files, ok := strings.Cut(e.Data, " "); ok {
				pairOnFiles(actual, l.Plan, me, strings.Split(files, ", "))
			}
		case EventStale:
			r.StaleEdits++
			pairOnFiles(actual, l.Plan, me, []string{e.Data})
		case "train_escalated", "hand_fix":
			if me > 0 {
				help[me] = true
			}
		}
	}
	r.TrainBounces = r.TrainConflicts + r.TestFailures
	r.PeakParallelism = peak
	for _, t := range l.Plan.Tickets {
		if help[t.Number] {
			r.NeededHelp = append(r.NeededHelp, t.Key)
		}
	}

	f := ForecastScore{Expected: l.Expected.Overlap, Predicted: l.Plan.Overlap}
	predicted := map[Pair]bool{}
	for _, p := range l.Plan.Pairs {
		if predictAt[p.Level] {
			predicted[Pair{A: p.A, B: p.B}] = true
		}
	}
	f.PredictedPairs, f.ActualPairs = sortedPairs(predicted), sortedPairs(actual)
	for _, p := range f.ActualPairs {
		if predicted[p] {
			f.Hits = append(f.Hits, p)
		} else {
			f.Misses = append(f.Misses, p)
		}
	}
	for _, p := range f.PredictedPairs {
		if !actual[p] {
			f.Unseen = append(f.Unseen, p)
		}
	}
	r.Forecast = f
	return r
}

// pairWith records a collision between ticket me and every other planned
// ticket ok accepts.
func pairWith(actual map[Pair]bool, p Plan, me int, ok func(PlannedTicket) bool) {
	if me == 0 {
		return
	}
	for _, o := range p.Tickets {
		if o.Number != me && ok(o) {
			actual[pairOf(me, o.Number)] = true
		}
	}
}

// pairOnFiles pairs me with every ticket whose edits touch one of files.
func pairOnFiles(actual map[Pair]bool, p Plan, me int, files []string) {
	want := map[string]bool{}
	for _, f := range files {
		if f = strings.TrimSpace(f); f != "" {
			want[claims.Clean(f)] = true
		}
	}
	pairWith(actual, p, me, func(o PlannedTicket) bool {
		for _, f := range o.Files {
			if want[f] {
				return true
			}
		}
		return false
	})
}

func pairOf(a, b int) Pair {
	if a > b {
		a, b = b, a
	}
	return Pair{A: fmt.Sprintf("#%d", a), B: fmt.Sprintf("#%d", b)}
}

func sortedPairs(m map[Pair]bool) []Pair {
	var out []Pair
	for p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].A != out[j].A {
			return out[i].A < out[j].A
		}
		return out[i].B < out[j].B
	})
	return out
}

func claimsCover(globs []string, p string) bool {
	for _, g := range globs {
		if claims.Match(g, p) {
			return true
		}
	}
	return false
}

// packagesOf is the packages ticket n changes plus those it imports.
func packagesOf(p Plan, n int) map[string]bool {
	out := map[string]bool{}
	for _, t := range p.Tickets {
		if t.Number != n {
			continue
		}
		for d := range dirsOf(t.Files) {
			out[d] = true
		}
		for _, imps := range t.Imports {
			for _, i := range imps {
				out[i] = true
			}
		}
	}
	return out
}

func dirsOf(files []string) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		out[path.Dir(f)] = true
	}
	return out
}

func overlaps(a, b map[string]bool) bool {
	for k := range a {
		if b[k] {
			return true
		}
	}
	return false
}

// Markdown is the report as a summary for people.
func Markdown(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Bench: %s\n\n", r.Scenario)
	fmt.Fprintf(&b, "Adapter %s, %s claims, %.1fs wall.", r.Adapter, r.Granularity, r.WallSeconds)
	if r.EndedEarly != "" {
		fmt.Fprintf(&b, " Ended early: %s.", r.EndedEarly)
	}
	b.WriteString("\n\n| Metric | Value |\n| --- | --- |\n")
	row := func(k, v string) { fmt.Fprintf(&b, "| %s | %s |\n", k, v) }
	row("Tickets landed", fmt.Sprintf("%d/%d", r.Landed, r.Tickets))
	row("Integration tests", map[bool]string{true: "pass", false: "FAIL"}[r.IntegrationGreen])
	row("Needed help", orDash(strings.Join(r.NeededHelp, ", ")))
	row("Denied writes", fmt.Sprint(r.DeniedWrites))
	row("Claim waits", fmt.Sprint(r.ClaimWaits))
	row("Train bounces", fmt.Sprintf("%d (%d conflicts, %d test failures)", r.TrainBounces, r.TrainConflicts, r.TestFailures))
	row("Restacks", fmt.Sprintf("%d (%d conflicts)", r.Restacks, r.RestackConflicts))
	row("Stale edits", fmt.Sprint(r.StaleEdits))
	row("Peak parallelism", fmt.Sprintf("%d (safe: %d)", r.PeakParallelism, r.SafeParallelism))
	tokens := "n/a"
	if r.Tokens != nil {
		tokens = fmt.Sprint(*r.Tokens)
	}
	row("Tokens", tokens)
	f := r.Forecast
	row("Forecast overlap", fmt.Sprintf("%s (expected %s)", f.Predicted, f.Expected))
	row("Predicted collisions", pairs(f.PredictedPairs))
	row("Actual collisions", pairs(f.ActualPairs))
	row("Hits / misses / unseen", fmt.Sprintf("%s / %s / %s", pairs(f.Hits), pairs(f.Misses), pairs(f.Unseen)))
	return b.String()
}

func pairs(ps []Pair) string {
	var s []string
	for _, p := range ps {
		s = append(s, p.A+"–"+p.B)
	}
	return orDash(strings.Join(s, ", "))
}

func orDash(s string) string {
	if s == "" {
		return "–"
	}
	return s
}
