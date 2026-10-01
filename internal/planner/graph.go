package planner

import (
	"slices"
	"strings"
)

// graph is a dependency graph over task indexes. An edge i -> j means j
// waits for i.
type graph struct {
	tasks []Task
	succ  []map[int][]string // succ[i][j] holds the reasons for edge i -> j
	pred  []map[int]bool
}

func newGraph(tasks []Task) *graph {
	g := &graph{tasks: tasks}
	for range tasks {
		g.succ = append(g.succ, map[int][]string{})
		g.pred = append(g.pred, map[int]bool{})
	}
	return g
}

// add records edge i -> j. Repeated edges are merged, keeping every distinct
// reason.
func (g *graph) add(i, j int, reason string) {
	if i == j || slices.Contains(g.succ[i][j], reason) {
		return
	}
	g.succ[i][j] = append(g.succ[i][j], reason)
	g.pred[j][i] = true
}

// intent ranks tasks in a topological order of the current edges, breaking
// ties by list order. Tasks stuck in a cycle are ranked last, in list order.
// It must be called after the After edges are added and before any others.
func (g *graph) intent() []int {
	n := len(g.tasks)
	indeg := make([]int, n)
	for j := range n {
		indeg[j] = len(g.pred[j])
	}
	rank := make([]int, n)
	done := make([]bool, n)
	for r := range n {
		next := -1
		for i := range n {
			if !done[i] && indeg[i] == 0 {
				next = i
				break
			}
		}
		if next < 0 {
			for i := range n {
				if !done[i] {
					next = i
					break
				}
			}
		}
		done[next] = true
		rank[next] = r
		for j := range g.succ[next] {
			indeg[j]--
		}
	}
	return rank
}

// edges lists every edge, sorted by From then To in list order.
func (g *graph) edges() []Edge {
	var out []Edge
	for i := range g.tasks {
		var js []int
		for j := range g.succ[i] {
			js = append(js, j)
		}
		slices.Sort(js)
		for _, j := range js {
			out = append(out, Edge{g.tasks[i].ID, g.tasks[j].ID, strings.Join(g.succ[i][j], "; ")})
		}
	}
	return out
}

// waves packs tasks into topological levels. Each wave holds at most limit
// tasks (no limit when limit <= 0), picked in rank order from those whose
// dependencies all ran in earlier waves. On a cycle it returns nil waves and
// the cycle as task IDs.
func (g *graph) waves(limit int, rank []int) ([][]string, []string) {
	n := len(g.tasks)
	indeg := make([]int, n)
	for j := range n {
		indeg[j] = len(g.pred[j])
	}
	placed := make([]bool, n)
	var out [][]string
	for left := n; left > 0; {
		var ready []int
		for i := range n {
			if !placed[i] && indeg[i] == 0 {
				ready = append(ready, i)
			}
		}
		if len(ready) == 0 {
			return nil, g.cycle(placed)
		}
		slices.SortFunc(ready, func(a, b int) int { return rank[a] - rank[b] })
		if limit > 0 && len(ready) > limit {
			ready = ready[:limit]
		}
		var wave []string
		for _, i := range ready {
			placed[i] = true
			wave = append(wave, g.tasks[i].ID)
		}
		for _, i := range ready {
			for j := range g.succ[i] {
				indeg[j]--
			}
		}
		out = append(out, wave)
		left -= len(ready)
	}
	return out, nil
}

// cycle finds a cycle among unplaced tasks. Each of them has an unplaced
// predecessor, so walking predecessors must revisit a task.
func (g *graph) cycle(placed []bool) []string {
	start := slices.Index(placed, false)
	seen := map[int]int{}
	var walk []int
	for at := start; ; {
		if k, ok := seen[at]; ok {
			walk = walk[k:]
			break
		}
		seen[at] = len(walk)
		walk = append(walk, at)
		next := -1
		for p := range g.pred[at] {
			if !placed[p] && (next < 0 || p < next) {
				next = p
			}
		}
		at = next
	}
	// walk follows edges backwards; report it forwards, from its first task
	// in list order, closed.
	slices.Reverse(walk)
	k := slices.Index(walk, slices.Min(walk))
	walk = append(walk[k:], walk[:k]...)
	ids := make([]string, 0, len(walk)+1)
	for _, i := range walk {
		ids = append(ids, g.tasks[i].ID)
	}
	return append(ids, ids[0])
}
