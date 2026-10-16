package forge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cliNames are the expressions that name a forge CLI.
var cliNames = map[string]bool{
	`"gh"`: true, `"glab"`: true,
	"config.ForgeGH": true, "config.ForgeGlab": true, "forge.GH": true, "forge.Glab": true,
}

// directExecs lists the exec.Command and exec.CommandContext calls in f
// that start a forge CLI.
func directExecs(fset *token.FileSet, f *ast.File) []token.Position {
	var hits []token.Position
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "exec" {
			return true
		}
		arg := map[string]int{"Command": 0, "CommandContext": 1}
		i, ok := arg[sel.Sel.Name]
		if !ok || len(call.Args) <= i {
			return true
		}
		if cliNames[exprString(call.Args[i])] {
			hits = append(hits, fset.Position(call.Pos()))
		}
		return true
	})
	return hits
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.BasicLit:
		return x.Value
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return id.Name + "." + x.Sel.Name
		}
	}
	return ""
}

// TestNoDirectCLIExec enforces the forge layer (docs/adr/0001-forge-layer.md):
// no package but internal/forge starts gh or glab itself.
func TestNoDirectCLIExec(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	self := filepath.Join(root, "internal", "forge")
	fset := token.NewFileSet()
	var hits []string
	files := 0
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "vendor" || p == self) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		for _, h := range directExecs(fset, f) {
			hits = append(hits, h.String())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 100 {
		t.Fatalf("scanned only %d files under %s; the walk is broken", files, root)
	}
	if len(hits) > 0 {
		t.Fatalf("these start a forge CLI outside internal/forge; call forge.Output, forge.CLI or a forge.Forge method instead:\n%s",
			strings.Join(hits, "\n"))
	}
}

func TestDirectExecsFlagsEachForm(t *testing.T) {
	src := `package x
import ("os/exec"; "context")
func f(ctx context.Context, name string) {
	exec.Command("gh", "pr", "list")
	exec.CommandContext(ctx, "glab", "mr")
	exec.Command(config.ForgeGH)
	exec.Command("git", "status")
	exec.Command(name)
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	hits := directExecs(fset, f)
	var lines []int
	for _, h := range hits {
		lines = append(lines, h.Line)
	}
	if len(lines) != 3 || lines[0] != 4 || lines[1] != 5 || lines[2] != 6 {
		t.Fatalf("directExecs = %v, want lines 4, 5 and 6", hits)
	}
}
