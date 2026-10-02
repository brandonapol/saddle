package planner

import (
	"reflect"
	"strings"
	"testing"
)

func issues(t Task, refs ...string) Task { t.Issues = refs; return t }

func titled(t Task, title string) Task { t.Title = title; return t }

func TestClusterEmpty(t *testing.T) {
	got, err := Cluster(nil, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("Cluster(nil) = %v, %v; want no stacks", got, err)
	}
}

func TestClusterSingle(t *testing.T) {
	got, err := Cluster([]Task{task("a", "internal/a/**")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !reflect.DeepEqual(got[0].Tasks, []string{"a"}) {
		t.Fatalf("got %+v; want one stack [a]", got)
	}
	if !strings.Contains(got[0].Reason, "standalone") {
		t.Errorf("reason %q; want it to say standalone", got[0].Reason)
	}
}

func TestCluster(t *testing.T) {
	tests := []struct {
		name    string
		tasks   []Task
		serial  []string
		stacks  [][]string
		reasons []string // substring per stack
		names   []string // optional, per stack
	}{
		{
			name:    "overlapping claims cluster together",
			tasks:   []Task{task("a", "internal/x/**"), task("b", "cmd/**"), task("c", "internal/x/y.go")},
			stacks:  [][]string{{"a", "c"}, {"b"}},
			reasons: []string{"claims overlap: a internal/x/** and c internal/x/y.go", "standalone"},
		},
		{
			name:    "disjoint tasks stay separate",
			tasks:   []Task{task("a", "internal/a/**"), task("b", "internal/b/**"), task("c", "docs/x.md")},
			stacks:  [][]string{{"a"}, {"b"}, {"c"}},
			reasons: []string{"standalone", "standalone", "standalone"},
		},
		{
			name: "shared issue groups disjoint claims",
			tasks: []Task{
				issues(task("a", "internal/a/**"), "#54"),
				task("b", "internal/b/**"),
				issues(task("c", "internal/c/**"), "54"),
			},
			stacks:  [][]string{{"a", "c"}, {"b"}},
			reasons: []string{"a and c share issue #54", "standalone"},
		},
		{
			name: "shared epic among several refs groups",
			tasks: []Task{
				issues(task("a", "internal/a/**"), "#54", "#6"),
				issues(task("b", "internal/b/**"), "owner/repo#7", "#6"),
			},
			stacks:  [][]string{{"a", "b"}},
			reasons: []string{"share issue #6"},
		},
		{
			name: "dependency edge groups and orders the stack",
			tasks: []Task{
				after(task("a", "internal/a/**"), "b"),
				task("b", "internal/b/**"),
			},
			stacks:  [][]string{{"b", "a"}},
			reasons: []string{"a runs after b"},
		},
		{
			name: "clusters are transitive",
			tasks: []Task{
				task("a", "internal/a/**"),
				issues(task("b", "internal/b/**"), "#1"),
				task("c", "internal/a/z.go"),
				issues(task("d", "internal/d/**"), "#1"),
				after(task("e", "internal/e/**"), "c"),
			},
			stacks:  [][]string{{"a", "c", "e"}, {"b", "d"}},
			reasons: []string{"claims overlap: a internal/a/** and c internal/a/z.go; e runs after c", "b and d share issue #1"},
		},
		{
			name:    "serial claims do not cluster",
			tasks:   []Task{task("a", "go.mod", "internal/a/**"), task("b", "go.mod", "internal/b/**")},
			serial:  []string{"go.mod", "go.sum"},
			stacks:  [][]string{{"a"}, {"b"}},
			reasons: []string{"standalone", "standalone"},
		},
		{
			name: "stack name comes from a shared title topic",
			tasks: []Task{
				titled(task("a", "internal/planner/**"), "Planner: clustering"),
				titled(task("b", "internal/planner/x.go"), "planner: fix x"),
				titled(task("c", "internal/tui/**"), "TUI: usage strip"),
				titled(task("d", "internal/e/**"), "no topic here"),
			},
			stacks:  [][]string{{"a", "b"}, {"c"}, {"d"}},
			reasons: []string{"claims overlap", "standalone", "standalone"},
			names:   []string{"planner", "tui", "d"},
		},
		{
			name: "stacks are ordered by their first task in planner intent",
			tasks: []Task{
				after(task("a", "internal/a/**"), "d"),
				task("b", "internal/b/**"),
				task("c", "internal/b/c.go"),
				task("d", "internal/d/**"),
			},
			stacks:  [][]string{{"b", "c"}, {"d", "a"}},
			reasons: []string{"claims overlap", "a runs after d"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Cluster(tc.tasks, tc.serial)
			if err != nil {
				t.Fatal(err)
			}
			var ids [][]string
			for _, s := range got {
				ids = append(ids, s.Tasks)
			}
			if !reflect.DeepEqual(ids, tc.stacks) {
				t.Fatalf("stacks = %v; want %v (%+v)", ids, tc.stacks, got)
			}
			for i, s := range got {
				if !strings.Contains(s.Reason, tc.reasons[i]) {
					t.Errorf("stack %d reason %q; want it to contain %q", i, s.Reason, tc.reasons[i])
				}
				if tc.names != nil && s.Name != tc.names[i] {
					t.Errorf("stack %d name %q; want %q", i, s.Name, tc.names[i])
				}
			}
		})
	}
}

func TestClusterDeterministic(t *testing.T) {
	tasks := []Task{
		issues(task("a", "internal/a/**"), "#3", "#9"),
		task("b", "internal/b/**", "internal/a/x.go"),
		issues(task("c", "internal/c/**"), "#9"),
		after(task("d", "internal/d/**"), "c"),
		task("e", "internal/e/**"),
		issues(task("f", "internal/f/**"), "#3"),
	}
	first, err := Cluster(tasks, nil)
	if err != nil {
		t.Fatal(err)
	}
	for range 50 {
		got, err := Cluster(tasks, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("run differs:\n%+v\nvs\n%+v", got, first)
		}
	}
}

func TestClusterErrors(t *testing.T) {
	for name, tasks := range map[string][]Task{
		"missing id":    {task("", "a/**")},
		"duplicate id":  {task("a"), task("a")},
		"unknown after": {after(task("a"), "zz")},
	} {
		if _, err := Cluster(tasks, nil); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}
