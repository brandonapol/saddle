package prgraph

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The package moves to its own module later (ADR 0009 §5): it imports only
// the standard library, and its non-test source never mentions the host's
// own concepts.
func TestImportBoundary(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := regexp.MustCompile(`(?i)\b(agents?|tasks?|tmux|claude|adapters?|notices?|decision cards?|saddle)\b`)
	for _, f := range files {
		test := strings.HasSuffix(f, "_test.go")
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if test && path == "modernc.org/sqlite" {
				continue // the test-only SQL driver for Migrate
			}
			if first := strings.Split(path, "/")[0]; strings.Contains(first, ".") {
				t.Errorf("%s imports %s; prgraph imports only the standard library", f, path)
			}
		}
		if test {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			if m := forbidden.FindString(line); m != "" {
				t.Errorf("%s:%d mentions %q: prgraph knows nothing of its host", f, i+1, m)
			}
		}
	}
}
