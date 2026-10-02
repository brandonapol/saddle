package gate

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/claims"
)

// Hold keeps Task from writing Paths until Until is ready. A hold with no
// Paths covers the whole task.
type Hold struct {
	ID      string
	Task    string
	Paths   []string // claim globs
	Until   Cond
	Created time.Time
	Note    string // why the hold was placed, for display
}

// Covers reports whether the hold applies to a write of path.
func (h Hold) Covers(path string) bool {
	if len(h.Paths) == 0 {
		return true
	}
	for _, g := range h.Paths {
		if claims.Match(g, path) {
			return true
		}
	}
	return false
}

// Status is a hold together with its evaluated condition.
type Status struct {
	Hold Hold
	Result
}

// Message is the text a denied write shows the agent.
func (s Status) Message() string { return "held (" + s.Hold.ID + "): " + s.Reason }

// Holds is a set of holds kept in ID order. The zero value is empty and ready
// to use. It is not safe for concurrent use.
type Holds struct{ hs []Hold }

// Add registers h. IDs must be unique and every hold needs a condition.
func (r *Holds) Add(h Hold) error {
	switch {
	case h.ID == "":
		return errors.New("gate: hold has no id")
	case h.Until == nil:
		return fmt.Errorf("gate: hold %s has no condition", h.ID)
	}
	i, found := r.find(h.ID)
	if found {
		return fmt.Errorf("gate: hold %s already exists", h.ID)
	}
	r.hs = slices.Insert(r.hs, i, h)
	return nil
}

// Remove drops the hold with id, reporting whether it existed.
func (r *Holds) Remove(id string) bool {
	i, found := r.find(id)
	if found {
		r.hs = slices.Delete(r.hs, i, i+1)
	}
	return found
}

// Get returns the hold with id.
func (r *Holds) Get(id string) (Hold, bool) {
	if i, found := r.find(id); found {
		return r.hs[i], true
	}
	return Hold{}, false
}

// List returns every hold in ID order.
func (r *Holds) List() []Hold { return slices.Clone(r.hs) }

func (r *Holds) find(id string) (int, bool) {
	return slices.BinarySearchFunc(r.hs, id, func(h Hold, id string) int { return strings.Compare(h.ID, id) })
}

// Evaluate evaluates every hold, in ID order.
func (r *Holds) Evaluate(now time.Time, s State) []Status {
	out := make([]Status, len(r.hs))
	for i, h := range r.hs {
		out[i] = Status{h, h.Until.Eval(now, s)}
	}
	return out
}

// Release removes every hold whose condition is ready and returns them.
func (r *Holds) Release(now time.Time, s State) []Status {
	var out []Status
	keep := r.hs[:0]
	for _, st := range r.Evaluate(now, s) {
		if st.Ready {
			out = append(out, st)
		} else {
			keep = append(keep, st.Hold)
		}
	}
	r.hs = keep
	return out
}

// Blocking returns the first unmet hold, in ID order, that stops task from
// writing path.
func (r *Holds) Blocking(task, path string, now time.Time, s State) (Status, bool) {
	for _, h := range r.hs {
		if h.Task != task || !h.Covers(path) {
			continue
		}
		if res := h.Until.Eval(now, s); !res.Ready {
			return Status{h, res}, true
		}
	}
	return Status{}, false
}

// Cycles finds holds that wait on each other: hold A waits on hold B when
// A's condition depends on B's task. Each cycle lists its hold IDs sorted;
// cycles are ordered by their first ID. A hold that waits on its own task is
// a cycle of one. Breaking any hold in a cycle unblocks the rest.
func (r *Holds) Cycles() [][]string {
	byTask := map[string][]int{}
	for i, h := range r.hs {
		byTask[h.Task] = append(byTask[h.Task], i)
	}
	edges := make([][]int, len(r.hs))
	self := make([]bool, len(r.hs))
	for i, h := range r.hs {
		for _, d := range h.Until.Deps() {
			for _, j := range byTask[d] {
				edges[i] = append(edges[i], j)
				self[i] = self[i] || i == j
			}
		}
	}

	// Tarjan's strongly connected components, visiting in ID order.
	n := len(r.hs)
	index, low := make([]int, n), make([]int, n)
	onStack := make([]bool, n)
	for i := range index {
		index[i] = -1
	}
	var stack []int
	var out [][]string
	next := 0
	var visit func(int)
	visit = func(v int) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range edges[v] {
			if index[w] < 0 {
				visit(w)
				low[v] = min(low[v], low[w])
			} else if onStack[w] {
				low[v] = min(low[v], index[w])
			}
		}
		if low[v] != index[v] {
			return
		}
		var ids []string
		for {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[w] = false
			ids = append(ids, r.hs[w].ID)
			if w == v {
				break
			}
		}
		if len(ids) > 1 || self[v] {
			slices.Sort(ids)
			out = append(out, ids)
		}
	}
	for v := range n {
		if index[v] < 0 {
			visit(v)
		}
	}
	slices.SortFunc(out, func(a, b []string) int { return strings.Compare(a[0], b[0]) })
	return out
}
