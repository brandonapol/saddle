package secretscan

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFindCatchesFlaggedShapes: the JWT fixture GitGuardian flagged in
// e21d593 (internal/remote/scrub_test.go), built here at runtime.
func TestFindCatchesFlaggedShapes(t *testing.T) {
	for _, s := range []string{
		"ey" + "JhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"gh" + "p_abcdefghijklmnopqrstuvwxyz0123456789",
		"AKI" + "AIOSFODNN7EXAMPLE",
		"xox" + "b-123456789012-abcdefghijkl",
	} {
		if got := Find([]byte("x := \"" + s + "\"")); len(got) != 1 {
			t.Errorf("Find(%q) = %q, want one match", s, got)
		}
	}
	if got := Find([]byte(`"ey" + "JhbGciOiJIUzI1NiJ9.eyJzdWIi..."`)); len(got) != 0 {
		t.Errorf("split literal flagged: %q", got)
	}
}

// TestNoSecretShapedLiteralsInSource walks the repo's Go and Markdown files.
func TestNoSecretShapedLiteralsInSource(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if ext := filepath.Ext(path); ext != ".go" && ext != ".md" {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range Find(b) {
			t.Errorf("%s holds a secret-shaped literal %q; build it at runtime instead", path, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
