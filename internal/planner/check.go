// Package planner turns a planned task list into a schedule.
//
// Check is the deterministic static checker that runs after the planner
// model. It doesn't call a model. It adds dependency edges where claims
// overlap, routes writes to serial globs through the merge train, flags
// structural moves as barriers, and packs the result into waves.
package planner

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/claims"
)

// Task is one planned unit of work. The order of tasks passed to Check is the
// planner's intent: when two tasks collide and nothing else orders them, the
// earlier one goes first.
type Task struct {
	ID     string
	Title  string
	Plan   string
	Claims []string // path globs the task may write
	After  []string // IDs of tasks the planner says must finish first
	Issues []string // optional issue or epic refs, "#54" or "owner/repo#54"
}

// Edge says task To can't start until task From has finished.
type Edge struct {
	From, To string
	Reason   string
}

// Flag marks a task for special handling, with a human-readable reason.
type Flag struct {
	Task   string
	Reason string
}

// Plan is the checker's output.
type Plan struct {
	Edges    []Edge     // sorted by From, then To
	Train    []Flag     // tasks that write serial globs, in landing order
	Barriers []Flag     // tasks that move or rename things, in planner order
	Waves    [][]string // task IDs per wave; nil when there is a cycle
}

// CycleError reports a dependency cycle. Cycle starts and ends on the same
// task, for example [a b c a].
type CycleError struct{ Cycle []string }

func (e *CycleError) Error() string {
	return "planner: dependency cycle: " + strings.Join(e.Cycle, " -> ")
}

var barrierWords = regexp.MustCompile(`(?i)\b(mov(e|es|ed|ing)|renam(e|es|ed|ing)|restructur(e|es|ed|ing))\b`)

// Check builds the edges, train routes, barriers and waves for tasks.
//
// Edges come from three sources, all ordered by planner intent:
//   - explicit After dependencies;
//   - claims that overlap: the task that comes first in planner intent runs first;
//   - barriers: a task whose title or plan mentions move, rename or restructure
//     is ordered against every other task, so it runs alone in its own wave.
//
// Planner intent is a topological order of the After graph, with ties going
// to list order. Every implicit edge follows it, so only After dependencies
// can form a cycle. Tasks with a
// claim that overlaps a serial glob go to the merge train. Train tasks are
// chained so that only one runs at a time.
//
// At most limit tasks share a wave; limit <= 0 means no limit. If the edges
// form a cycle, Check returns the plan without waves and a *CycleError.
func Check(tasks []Task, serial []string, limit int) (Plan, error) {
	g, err := afterGraph(tasks)
	if err != nil {
		return Plan{}, err
	}
	n := len(tasks)
	rank := g.intent()
	// before reports whether planner intent puts task i ahead of task j.
	before := func(i, j int) bool { return rank[i] < rank[j] }
	order := func(i, j int, reason string) {
		if before(i, j) {
			g.add(i, j, reason)
		} else {
			g.add(j, i, reason)
		}
	}

	var p Plan
	var barriers, train []int
	for i, t := range tasks {
		if w := barrierWords.FindString(t.Title + "\n" + t.Plan); w != "" {
			barriers = append(barriers, i)
			p.Barriers = append(p.Barriers, Flag{t.ID, fmt.Sprintf("barrier: plan mentions %q, so it runs alone", strings.ToLower(w))})
		}
		if hit := matching(t.Claims, serial); len(hit) > 0 {
			train = append(train, i)
		}
	}

	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if a, b, ok := firstOverlap(tasks[i].Claims, tasks[j].Claims); ok {
				first, second := tasks[i].ID, tasks[j].ID
				if !before(i, j) {
					first, second = second, first
					a, b = b, a
				}
				order(i, j, fmt.Sprintf("claims overlap: %s %s and %s %s; %s goes first by planner intent", first, a, second, b, first))
			}
		}
	}
	for _, b := range barriers {
		for j := 0; j < n; j++ {
			if j != b {
				order(b, j, fmt.Sprintf("barrier: %s restructures paths and runs alone", tasks[b].ID))
			}
		}
	}

	slices.SortFunc(train, func(a, b int) int { return rank[a] - rank[b] })
	for k, i := range train {
		hit := matching(tasks[i].Claims, serial)
		p.Train = append(p.Train, Flag{tasks[i].ID, "serial: writes " + strings.Join(hit, ", ") + "; the merge train lands it"})
		if k > 0 {
			prev := tasks[train[k-1]].ID
			order(train[k-1], i, fmt.Sprintf("serial: %s and %s both write serial paths; the train runs them one at a time", prev, tasks[i].ID))
		}
	}

	p.Edges = g.edges()
	waves, cyc := g.waves(limit, rank)
	if cyc != nil {
		return p, &CycleError{Cycle: cyc}
	}
	p.Waves = waves
	return p, nil
}

// afterGraph validates task IDs and After references and returns the graph of
// explicit After edges.
func afterGraph(tasks []Task) (*graph, error) {
	idx := make(map[string]int, len(tasks))
	for i, t := range tasks {
		if t.ID == "" {
			return nil, fmt.Errorf("planner: task %d has no id", i)
		}
		if _, dup := idx[t.ID]; dup {
			return nil, fmt.Errorf("planner: duplicate task id %q", t.ID)
		}
		idx[t.ID] = i
	}
	for _, t := range tasks {
		for _, a := range t.After {
			if _, ok := idx[a]; !ok {
				return nil, fmt.Errorf("planner: task %q is after unknown task %q", t.ID, a)
			}
		}
	}
	g := newGraph(tasks)
	for i, t := range tasks {
		for _, a := range t.After {
			g.add(idx[a], i, fmt.Sprintf("planner: %s runs after %s", t.ID, a))
		}
	}
	return g, nil
}

// matching returns the serial globs that any of mine overlaps.
func matching(mine, serial []string) []string {
	var hit []string
	for _, s := range serial {
		for _, c := range mine {
			if claims.Overlap(c, s) {
				hit = append(hit, s)
				break
			}
		}
	}
	return hit
}

func firstOverlap(as, bs []string) (string, string, bool) {
	for _, a := range as {
		for _, b := range bs {
			if claims.Overlap(a, b) {
				return a, b, true
			}
		}
	}
	return "", "", false
}
