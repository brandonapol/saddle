package forge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// notGHText are the functions outside internal/forge allowed to match on an
// error's text, because the text isn't gh's or glab's: git's, SQLite's, the
// OS's, or saddle's own. Key: repo-relative file and function. A forge
// failure is typed (Classify): branch on errors.Is, RefusalOf and
// RateLimitUntil instead (#822).
var notGHText = map[string]string{
	"internal/app/autopilot.go:(*autopilotEnv).Spawn":   "saddle's own concurrency-cap error",
	"internal/app/repairpending.go:PendingRepair.AtCap": "saddle's own concurrency-cap error",
	"internal/app/restackqueue.go:queuedRefusal":        "git push's GH006, not gh's",
	"internal/app/symassume.go:revBlob":                 "git's",
	"internal/automerge/retry.go:gitTransient":          "git's (push, index.lock), not gh's",
	"internal/gitx/changeset.go:pathMissing":            "git's",
	"internal/sentinel/sentinel.go:raced":               "git push's stale info",
	"internal/store/schema.go:backupBeforeMigrate":      "the OS's",
	"internal/store/watchers.go:(*Store).WatcherTicks":  "SQLite's",
}

var textMatchers = map[string]bool{
	"Contains": true, "ContainsAny": true, "HasPrefix": true, "HasSuffix": true, "Index": true, "EqualFold": true,
}

// errorTextMatches lists, as "file:func", each strings matcher in f whose
// argument is an error's text: an x.Error() call or a variable set from
// one.
func errorTextMatches(fset *token.FileSet, rel string, f *ast.File) []string {
	var hits []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		name := funcName(fn)
		fromErr := map[string]bool{} // variables holding error text
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.AssignStmt:
				for i, r := range x.Rhs {
					if i < len(x.Lhs) && mentionsErrorText(r, fromErr) {
						if id, ok := x.Lhs[i].(*ast.Ident); ok {
							fromErr[id.Name] = true
						}
					}
				}
			case *ast.CallExpr:
				sel, ok := x.Fun.(*ast.SelectorExpr)
				if !ok || !textMatchers[sel.Sel.Name] {
					return true
				}
				if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "strings" {
					return true
				}
				for _, a := range x.Args {
					if mentionsErrorText(a, fromErr) {
						hits = append(hits, rel+":"+name)
						break
					}
				}
			}
			return true
		})
	}
	return hits
}

// mentionsErrorText reports whether e calls .Error() or reads a variable
// that holds error text.
func mentionsErrorText(e ast.Expr, fromErr map[string]bool) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Error" && len(x.Args) == 0 {
				found = true
			}
		case *ast.Ident:
			if fromErr[x.Name] {
				found = true
			}
		}
		return !found
	})
	return found
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		if id, ok := s.X.(*ast.Ident); ok {
			return "(*" + id.Name + ")." + fn.Name.Name
		}
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// TestNoStderrMatchingOutsideForge fails when code outside internal/forge
// branches on a failure's text: gh's stderr is read in one place, Classify,
// and everyone else gets a typed outcome (#822).
func TestNoStderrMatchingOutsideForge(t *testing.T) {
	root := filepath.Join("..", "..")
	seen := map[string]bool{}
	var bad []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if rel == "internal/forge" || d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			for _, h := range errorTextMatches(fset, rel, f) {
				seen[h] = true
				if _, ok := notGHText[h]; !ok {
					bad = append(bad, h)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s matches on an error's text; use forge's typed outcomes (errors.Is, forge.RefusalOf), or add it to notGHText if the text isn't gh's", b)
	}
	for k := range notGHText {
		if !seen[k] {
			t.Errorf("notGHText lists %s, which no longer matches on error text: remove it", k)
		}
	}
}
