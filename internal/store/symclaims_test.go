package store

import (
	"slices"
	"testing"
)

func symNames(cs []SymbolClaim) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Task+":"+c.Symbol)
	}
	return out
}

// #591: ClaimSymbols replaces a task's rows on a path, refuses a symbol
// another live task holds, and rows of landed or killed tasks drop out of
// SymbolClaims and stop clashing.
func TestSymbolClaimsLifecycle(t *testing.T) {
	s := openTest(t)
	for _, id := range []string{"t1", "t2", "t3"} {
		if err := s.CreateTask(Task{ID: id, Title: id, Status: Running}); err != nil {
			t.Fatal(err)
		}
	}
	claim := func(task string, syms ...string) *SymbolClaim {
		t.Helper()
		var want []SymbolClaim
		for _, n := range syms {
			want = append(want, SymbolClaim{Task: task, Path: "x.go", Symbol: n, Kind: "func"})
		}
		clash, err := s.ClaimSymbols(task, "x.go", want)
		if err != nil {
			t.Fatal(err)
		}
		return clash
	}
	if c := claim("t1", "Foo", "Baz"); c != nil {
		t.Fatalf("first claim clashed with %+v", c)
	}
	if c := claim("t1", "Foo"); c != nil {
		t.Fatalf("own reclaim clashed with %+v", c)
	}
	if c := claim("t2", "Bar"); c != nil {
		t.Fatalf("disjoint claim clashed with %+v", c)
	}
	if c := claim("t3", "Qux", "Foo"); c == nil || c.Task != "t1" || c.Symbol != "Foo" {
		t.Fatalf("clash = %+v, want t1's Foo", c)
	}
	got, err := s.SymbolClaims("x.go")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"t1:Foo", "t2:Bar"}; !slices.Equal(symNames(got), want) {
		t.Fatalf("SymbolClaims = %q, want %q (t1's Baz replaced, t3's refused claim unwritten)", symNames(got), want)
	}
	if other, _ := s.SymbolClaims("y.go"); len(other) != 0 {
		t.Fatalf("y.go claims = %+v", other)
	}

	if err := s.SetStatus("t1", Landed); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("t2", Killed); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.SymbolClaims("x.go"); len(got) != 0 {
		t.Fatalf("SymbolClaims after land/kill = %q, want none", symNames(got))
	}
	if c := claim("t3", "Foo"); c != nil {
		t.Fatalf("landed task's row still clashes: %+v", c)
	}
}
