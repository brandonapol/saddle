package app

import "testing"

// #845: StackLayers says which layer each PR is stacked on, so the sentinel
// labels only the PRs that depend on a conflict. t2 builds on t1's a.txt and
// is stacked on it; t3 is independent and targets base.
func TestStackLayersBelow(t *testing.T) {
	t.Parallel()
	a := flagTwoStacks(t)
	ls, err := a.StackLayers()
	must(t, err)
	got := map[string]int{}
	for _, l := range ls {
		got[l.Task.ID] = l.Below
	}
	if want := map[string]int{"t1": -1, "t2": 0, "t3": -1}; len(got) != 3 || got["t1"] != want["t1"] || got["t2"] != want["t2"] || got["t3"] != want["t3"] {
		t.Fatalf("Below = %v, want %v", got, want)
	}
	if !StackedOn(ls, 1, 0) {
		t.Fatal("t2 isn't stacked on t1")
	}
	if StackedOn(ls, 2, 0) || StackedOn(ls, 2, 1) {
		t.Fatal("independent t3 is stacked on t1's stack")
	}
}
