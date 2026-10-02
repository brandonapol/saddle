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
		{"**/*.go", "web/x.ts", true}, // conservative: x.ts could be a directory
		{"**/*.go", "web/*.ts", false},
		{"go.sum", "go.mod", false},
		{"internal/app/automerge*.go", "internal/app/lifecycle.go", false},
		{"automerge*.go", "lifecycle.go", false},
		{"sync*.go", "briefs_test.go", false},
		{"sync*.go", "*_test.go", true},
		{"a*.go", "ab*.go", true},
		{"a*.go", "b*.go", false},
		{"*.go", "*.ts", false},
		{"**/x.go", "a/b/x.go", true},
		{"**/x.go", "a/b/y.go", true}, // y.go could be a directory
		{"**/x.go", "a/b/*.go", true},
		{"internal/**", "internal/app/x.go", true},
		{"internal/**", "cmd/**", false},
		{"internal/*/x.go", "internal/app/y.go", false},
		{"web/**/*.tsx", "web/a/*.ts", false},
		{"foo/*.go", "foo/bar.go", true},
		{"foo/*.go", "foo", true},
		{"a/{b,c}.go", "a/b.go", true},
		{"internal/{a,b}/**", "internal/c/x.go", true}, // conservative
		{"a/*.go", "a/*.go", true},
		{"internal/app/x.go", "internal/app/x.go", true},
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
