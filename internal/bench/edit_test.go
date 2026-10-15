package bench

import (
	"maps"
	"path"
	"reflect"
	"slices"
	"testing"

	"github.com/brandonapol/saddle/internal/forecast"
)

// mapTree is a Tree in memory.
type mapTree map[string]string

func (m mapTree) Read(rel string) (string, bool) { s, ok := m[rel]; return s, ok }

func (m mapTree) Files(dir string) []string {
	var out []string
	for p := range m {
		if d := path.Dir(p); d == dir || (dir == "" && d == ".") {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

func TestApplyKinds(t *testing.T) {
	base := mapTree{"a/x.go": "one\ntwo\n", "a/y.go": "keep\n", "gone.txt": "bye\n"}
	c := Apply(base, []Edit{
		{Path: "a/x.go", Old: "two", New: "TWO"},
		{Path: "a/x.go", Append: true, New: "three\n"},
		{Path: "a/z.go", From: "a/y.go"},
		{Path: "gone.txt", Remove: true},
		{Path: "new.txt", New: "hi\n"},
	})
	want := map[string]string{"a/x.go": "one\nTWO\nthree\n", "a/z.go": "keep\n", "new.txt": "hi\n"}
	if !maps.Equal(c.Writes, want) {
		t.Errorf("writes = %q, want %q", c.Writes, want)
	}
	if !slices.Equal(c.Removes, []string{"a/y.go", "gone.txt"}) {
		t.Errorf("removes = %v", c.Removes)
	}
	if !reflect.DeepEqual(c.Renames, []forecast.Rename{{Old: "a/y.go", New: "a/z.go"}}) {
		t.Errorf("renames = %v", c.Renames)
	}
	if !slices.Equal(c.Files(), []string{"a/x.go", "a/y.go", "a/z.go", "gone.txt", "new.txt"}) {
		t.Errorf("files = %v", c.Files())
	}
}

// An agent asked to edit code that moved finds it in the same package.
func TestApplyFollowsAMove(t *testing.T) {
	base := mapTree{"t/strings.go": "func Reverse() {\n\tbody\n}\n"}
	c := Apply(base, []Edit{{Path: "t/util.go", Old: "\tbody\n", New: "\tguard\n\tbody\n"}})
	if c.Writes["t/strings.go"] != "func Reverse() {\n\tguard\n\tbody\n}\n" || c.Followed["t/util.go"] != "t/strings.go" {
		t.Fatalf("change = %+v", c)
	}
	if len(c.Stale) > 0 {
		t.Fatalf("stale = %v", c.Stale)
	}
}

// When the anchor is nowhere, the edit is stale and skipped.
func TestApplyStaleAnchor(t *testing.T) {
	base := mapTree{"s/s.go": "return r.W * r.H\n"}
	c := Apply(base, []Edit{{Path: "s/s.go", Old: "return r.H * r.W\n", New: "x"}, {Path: "s/t.go", From: "s/missing.go"}})
	if len(c.Writes) != 0 || !slices.Equal(c.Stale, []string{"s/s.go", "s/missing.go"}) {
		t.Fatalf("change = %+v", c)
	}
}
