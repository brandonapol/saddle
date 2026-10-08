package runq

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// doubleAcquires lists the functions in f that call Acquire more than once.
// Holding one lease while waiting for another is the only deadlock the queue
// can't rule out (docs/runq.md Q8): nested runs must ride on EnvLease, never
// take a second lease of their own.
func doubleAcquires(fset *token.FileSet, f *ast.File) []string {
	var out []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		n := 0
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			if c, ok := node.(*ast.CallExpr); ok {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Acquire" {
					n++
				}
			}
			return true
		})
		if n > 1 {
			out = append(out, fset.Position(fn.Pos()).String()+" "+fn.Name.Name)
		}
	}
	return out
}

func usesRunq(f *ast.File, inRunq bool) bool {
	if inRunq {
		return true
	}
	for _, imp := range f.Imports {
		if strings.HasSuffix(strings.Trim(imp.Path.Value, `"`), "/internal/runq") {
			return true
		}
	}
	return false
}

func TestDoubleAcquireLintCatchesIt(t *testing.T) {
	src := `package x
func bad(q *Queue) { a, _ := q.Acquire(ctx, r1); b, _ := q.Acquire(ctx, r2); _, _ = a, b }
func good(q *Queue) { a, _ := q.Acquire(ctx, r1); _ = a }`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := doubleAcquires(fset, f)
	if len(got) != 1 || !strings.HasSuffix(got[0], " bad") {
		t.Fatalf("got %v", got)
	}
}

// TestNoCodeAcquiresTwoLeases fails if non-test code anywhere in the module
// takes two runq leases in one function.
func TestNoCodeAcquiresTwoLeases(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	here, _ := filepath.Abs(".")
	fset := token.NewFileSet()
	var bad []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); path != root && (strings.HasPrefix(n, ".") || n == "testdata" || n == "build") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		if usesRunq(f, filepath.Dir(path) == here) {
			bad = append(bad, doubleAcquires(fset, f)...)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) > 0 {
		t.Fatalf("functions that acquire two runq leases (hold-and-wait deadlock; ride on %s instead):\n%s", EnvLease, strings.Join(bad, "\n"))
	}
}
