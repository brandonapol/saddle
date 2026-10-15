package bench

import (
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
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

// Before the first spawn the integration branch doesn't exist yet; a
// task's base is then the base branch, which saddle cuts it from.
func TestSpawnBaseFallsBackToBase(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=b", "-c", "user.email=b@x", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	if got := SpawnBase(dir, "saddle/integration", "main"); got != (GitTree{Dir: dir, Rev: "main"}) {
		t.Fatalf("with no integration branch, base = %+v, want main", got)
	}
	if s, ok := SpawnBase(dir, "saddle/integration", "main").Read("a.txt"); !ok || s != "one\n" {
		t.Fatalf("read a.txt = %q, %v", s, ok)
	}
	git("branch", "saddle/integration")
	if got := SpawnBase(dir, "saddle/integration", "main"); got.Rev != "saddle/integration" {
		t.Fatalf("with an integration branch, base = %+v", got)
	}
}
