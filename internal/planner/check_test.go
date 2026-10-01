package planner

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func task(id string, claims ...string) Task { return Task{ID: id, Title: id, Claims: claims} }

func after(t Task, ids ...string) Task { t.After = ids; return t }

func plan(t Task, text string) Task { t.Plan = text; return t }

func TestCheck(t *testing.T) {
	type edge struct{ from, to, reason string } // reason is a substring
	tests := []struct {
		name     string
		tasks    []Task
		serial   []string
		limit    int
		edges    []edge
		train    []string
		barriers []string
		waves    [][]string
		cycle    []string
	}{
		{
			name:  "disjoint claims run together",
			tasks: []Task{task("a", "internal/a/**"), task("b", "internal/b/**")},
			waves: [][]string{{"a", "b"}},
		},
		{
			name:  "overlapping globs follow list order",
			tasks: []Task{task("a", "internal/x/**"), task("b", "internal/x/y.go"), task("c", "cmd/**")},
			edges: []edge{{"a", "b", "claims overlap: a internal/x/** and b internal/x/y.go; a goes first"}},
			waves: [][]string{{"a", "c"}, {"b"}},
		},
		{
			name:  "directory path covers files under it",
			tasks: []Task{task("a", "docs"), task("b", "docs/ARCHITECTURE.md")},
			edges: []edge{{"a", "b", "claims overlap"}},
			waves: [][]string{{"a"}, {"b"}},
		},
		{
			name:  "sibling prefixes do not overlap",
			tasks: []Task{task("a", "internal/claims/**"), task("b", "internal/claimsx/**")},
			waves: [][]string{{"a", "b"}},
		},
		{
			name: "explicit After overrides list order for overlaps",
			tasks: []Task{
				after(task("a", "pkg/**"), "b"),
				task("b", "pkg/x.go"),
			},
			edges: []edge{{"b", "a", "planner: a runs after b"}, {"b", "a", "claims overlap: b pkg/x.go and a pkg/**; b goes first"}},
			waves: [][]string{{"b"}, {"a"}},
		},
		{
			name:   "serial globs go to the train one at a time",
			serial: []string{"go.sum", "internal/store/migrations/**"},
			tasks: []Task{
				task("a", "internal/a/**", "go.mod", "go.sum"),
				task("b", "internal/b/**"),
				task("c", "internal/store/migrations/0002.sql"),
			},
			edges: []edge{{"a", "c", "serial: a and c both write serial paths"}},
			train: []string{"a", "c"},
			waves: [][]string{{"a", "b"}, {"c"}},
		},
		{
			name:   "train follows planner intent",
			serial: []string{"go.sum"},
			tasks: []Task{
				after(task("a", "go.sum", "x/**"), "b"),
				task("b", "go.sum", "y/**"),
			},
			edges: []edge{{"b", "a", "serial: b and a"}},
			train: []string{"b", "a"},
			waves: [][]string{{"b"}, {"a"}},
		},
		{
			name: "barrier runs alone",
			tasks: []Task{
				task("a", "internal/a/**"),
				plan(task("mv", "internal/old/**", "internal/new/**"), "Rename internal/old to internal/new."),
				task("b", "internal/b/**"),
				task("c", "internal/c/**"),
			},
			edges: []edge{
				{"a", "mv", "barrier: mv restructures paths"},
				{"mv", "b", "barrier"},
				{"mv", "c", "barrier"},
			},
			barriers: []string{"mv"},
			waves:    [][]string{{"a"}, {"mv"}, {"b", "c"}},
		},
		{
			name: "barrier first in the list goes first",
			tasks: []Task{
				{ID: "r", Title: "Restructure the cli package", Claims: []string{"internal/cli/**"}},
				task("a", "internal/a/**"),
				task("b", "internal/b/**"),
			},
			barriers: []string{"r"},
			waves:    [][]string{{"r"}, {"a", "b"}},
		},
		{
			name: "barrier words need a whole word",
			tasks: []Task{
				plan(task("a", "a/**"), "Remove the dead code and improve errors."),
				task("b", "b/**"),
			},
			waves: [][]string{{"a", "b"}},
		},
		{
			name:  "limit packs waves in planner order",
			limit: 2,
			tasks: []Task{task("a", "a/**"), task("b", "b/**"), task("c", "c/**"), task("d", "d/**"), task("e", "e/**")},
			waves: [][]string{{"a", "b"}, {"c", "d"}, {"e"}},
		},
		{
			name:  "deferred tasks keep their place ahead of newly ready ones",
			limit: 2,
			tasks: []Task{
				task("a", "a/**"),
				task("b", "b/**"),
				task("c", "c/**"),
				after(task("d", "d/**"), "a"),
			},
			edges: []edge{{"a", "d", "planner"}},
			waves: [][]string{{"a", "b"}, {"c", "d"}},
		},
		{
			name: "explicit cycle is reported",
			tasks: []Task{
				after(task("a", "a/**"), "c"),
				after(task("b", "b/**"), "a"),
				after(task("c", "c/**"), "b"),
				task("d", "d/**"),
			},
			edges: []edge{{"c", "a", "planner"}, {"a", "b", "planner"}, {"b", "c", "planner"}},
			cycle: []string{"a", "b", "c", "a"},
		},
		{
			name: "overlaps never create a cycle on their own",
			tasks: []Task{
				task("a", "x/**", "y/**"),
				after(task("b", "y/1.go"), "c"),
				task("c", "x/1.go"),
			},
			edges: []edge{{"a", "b", "claims overlap"}, {"a", "c", "claims overlap"}, {"c", "b", "planner"}},
			waves: [][]string{{"a"}, {"c"}, {"b"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := Check(tt.tasks, tt.serial, tt.limit)
			if tt.cycle != nil {
				var ce *CycleError
				if !errors.As(err, &ce) {
					t.Fatalf("err = %v, want a CycleError", err)
				}
				if !reflect.DeepEqual(ce.Cycle, tt.cycle) {
					t.Errorf("cycle = %v, want %v", ce.Cycle, tt.cycle)
				}
			} else if err != nil {
				t.Fatal(err)
			}

			for _, w := range tt.edges {
				if !hasEdge(p.Edges, w.from, w.to, w.reason) {
					t.Errorf("missing edge %s -> %s with reason %q; edges: %+v", w.from, w.to, w.reason, p.Edges)
				}
			}
			// Barriers add an edge to every task; only check the count elsewhere.
			if len(tt.barriers) == 0 {
				pairs := map[[2]string]bool{}
				for _, w := range tt.edges {
					pairs[[2]string{w.from, w.to}] = true
				}
				if len(p.Edges) != len(pairs) {
					t.Errorf("got %d edges, want %d: %+v", len(p.Edges), len(pairs), p.Edges)
				}
			}
			if got := ids(p.Train); !reflect.DeepEqual(got, tt.train) {
				t.Errorf("train = %v, want %v", got, tt.train)
			}
			if got := ids(p.Barriers); !reflect.DeepEqual(got, tt.barriers) {
				t.Errorf("barriers = %v, want %v", got, tt.barriers)
			}
			if !reflect.DeepEqual(p.Waves, tt.waves) {
				t.Errorf("waves = %v, want %v", p.Waves, tt.waves)
			}
			for _, f := range append(p.Train, p.Barriers...) {
				if f.Reason == "" {
					t.Errorf("flag for %s has no reason", f.Task)
				}
			}
		})
	}
}

func TestCheckBarrierEdgesCoverEveryTask(t *testing.T) {
	tasks := []Task{
		task("a", "a/**"),
		plan(task("mv", "m/**"), "Move m into n."),
		task("b", "b/**"),
	}
	p, err := Check(tasks, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Edges) != 2 {
		t.Errorf("edges = %+v, want a -> mv and mv -> b", p.Edges)
	}
	if !strings.Contains(p.Barriers[0].Reason, `"move"`) {
		t.Errorf("barrier reason = %q, want it to name the word", p.Barriers[0].Reason)
	}
}

func TestCheckInvalidInput(t *testing.T) {
	tests := []struct {
		name  string
		tasks []Task
		want  string
	}{
		{"empty id", []Task{{}}, "no id"},
		{"duplicate id", []Task{task("a"), task("a")}, "duplicate"},
		{"unknown after", []Task{after(task("a"), "zz")}, "unknown task"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Check(tt.tasks, nil, 0); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestOverlap(t *testing.T) {
	tests := []struct {
		a, b string
		want bool
	}{
		{"internal/a/**", "internal/a/x.go", true},
		{"internal/a/**", "internal/b/**", false},
		{"internal/a", "internal/a/x/y.go", true},
		{"./internal/a/", "internal/a", true},
		{"go.sum", "go.sum", true},
		{"go.sum", "go.mod", false},
		{"**/*.go", "cmd/main.go", true},
		{"internal/*.go", "cmd/main.go", false},
		{"internal/{a,b}/**", "internal/c/x.go", true}, // conservative
	}
	for _, tt := range tests {
		if got := overlap(tt.a, tt.b); got != tt.want {
			t.Errorf("overlap(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
		if got := overlap(tt.b, tt.a); got != tt.want {
			t.Errorf("overlap(%q, %q) = %v, want %v", tt.b, tt.a, got, tt.want)
		}
	}
}

func hasEdge(es []Edge, from, to, reason string) bool {
	for _, e := range es {
		if e.From == from && e.To == to && strings.Contains(e.Reason, reason) {
			return true
		}
	}
	return false
}

func ids(fs []Flag) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Task)
	}
	return out
}
