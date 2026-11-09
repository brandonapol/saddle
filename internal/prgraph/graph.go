package prgraph

import "sort"

// graph is the dependency graph Reconcile honours: edges to known changes
// that aren't superseded (a dependent of a superseded change loses that
// edge), with no self-edges and no cycles. Edges are added in (change seq,
// change id, dep id) order and one that would close a cycle is dropped, so
// the result doesn't depend on the input's order.
type graph struct {
	deps map[string][]string // honoured deps, sorted
	byID map[string]*Change
	idx  map[string]int
	anc  [][]bool // anc[i][j]: change i depends on change j, transitively
}

func buildGraph(changes []Change, edges []Edge, inBase map[string]bool) *graph {
	g := &graph{deps: map[string][]string{}, byID: map[string]*Change{}, idx: map[string]int{}}
	for i := range changes {
		g.byID[changes[i].ID] = &changes[i]
		g.idx[changes[i].ID] = i
	}
	g.anc = make([][]bool, len(changes))
	for i := range g.anc {
		g.anc[i] = make([]bool, len(changes))
	}
	es := append([]Edge(nil), edges...)
	sort.Slice(es, func(i, j int) bool {
		a, b := es[i], es[j]
		ca, cb := g.byID[a.Change], g.byID[b.Change]
		if ca != nil && cb != nil && ca.Seq != cb.Seq {
			return ca.Seq < cb.Seq
		}
		if a.Change != b.Change {
			return a.Change < b.Change
		}
		return a.Dep < b.Dep
	})
	for _, e := range es {
		d, c := g.byID[e.Dep], g.byID[e.Change]
		if d == nil || c == nil || e.Dep == e.Change {
			continue
		}
		if d.State == Superseded && !inBase[d.ID] {
			continue
		}
		ci, di := g.idx[e.Change], g.idx[e.Dep]
		if containsStr(g.deps[e.Change], e.Dep) || g.anc[di][ci] {
			continue // a duplicate, or an edge that would close a cycle
		}
		g.deps[e.Change] = append(g.deps[e.Change], e.Dep)
		// Everything that reaches c (and c itself) now reaches d and d's
		// ancestors.
		for x := range g.anc {
			if x != ci && !g.anc[x][ci] {
				continue
			}
			g.anc[x][di] = true
			for y, ok := range g.anc[di] {
				if ok {
					g.anc[x][y] = true
				}
			}
		}
	}
	for k := range g.deps {
		sort.Strings(g.deps[k])
	}
	return g
}

// closure returns every change id depends on, transitively, sorted.
func (g *graph) closure(id string) []string {
	i, ok := g.idx[id]
	if !ok {
		return nil
	}
	var out []string
	for other, j := range g.idx {
		if g.anc[i][j] {
			out = append(out, other)
		}
	}
	sort.Strings(out)
	return out
}

// depth is the longest chain of honoured edges below id.
func (g *graph) depth(id string, memo map[string]int) int {
	if v, ok := memo[id]; ok {
		return v
	}
	memo[id] = 0
	best := 0
	for _, d := range g.deps[id] {
		if v := g.depth(d, memo) + 1; v > best {
			best = v
		}
	}
	memo[id] = best
	return best
}

func containsStr(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
