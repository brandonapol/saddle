package server

import "testing"

func TestServe(t *testing.T) {
	if got, ok := Serve("/hello"); !ok || got != "hello" {
		t.Fatalf("Serve(/hello) = %q, %v", got, ok)
	}
	if _, ok := Serve("/missing"); ok {
		t.Fatal("Serve(/missing) found a route")
	}
}

func TestRoutesUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Routes {
		if seen[r.Path] {
			t.Fatalf("route %s registered twice", r.Path)
		}
		seen[r.Path] = true
	}
}
