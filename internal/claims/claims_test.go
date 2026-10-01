package claims

import "testing"

func TestMatch(t *testing.T) {
	cases := []struct {
		glob, path string
		want       bool
	}{
		{"pkg/billing/**", "pkg/billing/meter/x.go", true},
		{"pkg/billing", "pkg/billing/meter/x.go", true},
		{"pkg/billing/", "pkg/billing/x.go", true},
		{"pkg/bill", "pkg/billing/x.go", false},
		{"go.sum", "go.sum", true},
		{"*.md", "README.md", true},
		{"web/**/*.tsx", "web/a/b.tsx", true},
		{"web/**/*.tsx", "web/a/b.ts", false},
	}
	for _, c := range cases {
		if got := Match(c.glob, c.path); got != c.want {
			t.Errorf("Match(%q, %q) = %v, want %v", c.glob, c.path, got, c.want)
		}
	}
}

func TestOverlap(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"pkg/billing/**", "pkg/billing/invoice/**", true},
		{"pkg/billing/meter/**", "pkg/billing/invoice/**", false},
		{"pkg/billing/meter", "pkg/billing/meter/x.go", true},
		{"internal/stripe/**", "web/**", false},
		{"**/*.go", "web/x.ts", true}, // conservative
		{"go.sum", "go.mod", false},
	}
	for _, c := range cases {
		if got := Overlap(c.a, c.b); got != c.want {
			t.Errorf("Overlap(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestRemap(t *testing.T) {
	rs := []Rename{
		{"billing/meter/a.go", "pkg/billing/meter/a.go"},
		{"billing/meter/b.go", "pkg/billing/meter/b.go"},
	}
	if got := Remap("billing/meter/**", rs); got != "pkg/billing/meter/**" {
		t.Errorf("Remap dir = %q", got)
	}
	if got := Remap("web/**", rs); got != "web/**" {
		t.Errorf("Remap untouched = %q", got)
	}
	if got := Remap("billing/meter/a.go", rs); got != "pkg/billing/meter/a.go" {
		t.Errorf("Remap file = %q", got)
	}
}
