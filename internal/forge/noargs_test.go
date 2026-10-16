package forge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ghCommands are gh's subcommands by command group, as saddle uses them. A
// call or slice whose string arguments run "pr", "view" (or "api" and an
// endpoint) is building gh arguments.
var ghCommands = map[string][]string{
	"pr":    {"create", "view", "list", "edit", "merge", "close", "comment", "checks", "reopen", "ready"},
	"issue": {"create", "view", "list", "edit", "close", "comment", "reopen"},
	"run":   {"view", "rerun", "list", "watch"},
	"label": {"create", "list", "edit", "delete"},
	"auth":  {"status"},
	"repo":  {"view"},
	"api":   nil, // any endpoint
}

// ghArgsAllowed are the paths, relative to the module root, that may still
// build gh arguments, and why. A directory covers everything under it.
var ghArgsAllowed = map[string]string{
	"internal/forge":   "the forge layer itself",
	"internal/ghstack": "the gh-stack extension, GitHub-only by design (no GitLab equivalent): ghstack.For gates it on the forge's provider",
	"internal/e2e":     "the fake gh binary and the journeys that drive it",
	"internal/bench":   "drives the fake gh",
}

// ghArgs lists where f builds gh arguments: two string literals in a row in
// a call's arguments or a slice literal that name a gh command.
func ghArgs(fset *token.FileSet, f *ast.File) []token.Position {
	var hits []token.Position
	scan := func(args []ast.Expr) {
		for i := 0; i+1 < len(args); i++ {
			cmd, ok1 := stringLit(args[i])
			sub, ok2 := stringLit(args[i+1])
			if !ok1 || !ok2 {
				continue
			}
			subs, ok := ghCommands[cmd]
			if !ok {
				continue
			}
			if cmd == "api" && (strings.HasPrefix(sub, "repos/") || sub == "graphql" || sub == "rate_limit") ||
				cmd != "api" && containsString(subs, sub) {
				hits = append(hits, fset.Position(args[i].Pos()))
				return
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			scan(x.Args)
		case *ast.CompositeLit:
			scan(x.Elts)
		}
		return true
	})
	return hits
}

// stringLit is e's string, or for "a"+x the string it starts with.
func stringLit(e ast.Expr) (string, bool) {
	if bin, ok := e.(*ast.BinaryExpr); ok && bin.Op == token.ADD {
		return stringLit(bin.X)
	}
	b, ok := e.(*ast.BasicLit)
	if !ok || b.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(b.Value)
	return s, err == nil
}

func containsString(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func ghArgsAllowedAt(rel string) bool {
	for p := range ghArgsAllowed {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// TestNoGHArgsOutsideForge enforces #647: only internal/forge (and the
// GitHub-only exceptions in ghArgsAllowed) builds gh arguments. Everything
// else calls a forge.Forge operation, so [forge] applies everywhere and a
// GitLab forge can stand in.
func TestNoGHArgsOutsideForge(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s: %v", root, err)
	}
	fset := token.NewFileSet()
	var hits []string
	files := 0
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if p != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata" || d.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") || ghArgsAllowedAt(rel) {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		files++
		for _, h := range ghArgs(fset, f) {
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
		t.Fatalf("these build gh arguments outside internal/forge; add a forge.Forge operation and call it instead:\n%s",
			strings.Join(hits, "\n"))
	}
}

func TestGHArgsFlagsEachForm(t *testing.T) {
	src := `package x
func f() {
	run(ctx, "pr", "view", url, "--json", "state")
	forge.Output(ctx, dir, forge.GH, "issue", "create")
	args := []string{"pr", "merge", ref}
	g.Run("api", "repos/"+repo+"/issues")
	git("log", "--oneline")
	run("pr", ref)
	tmux("run", "-t", "x")
	_ = []string{"api", "key"}
}`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "x.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	for _, h := range ghArgs(fset, f) {
		lines = append(lines, h.Line)
	}
	if len(lines) != 4 || lines[0] != 3 || lines[1] != 4 || lines[2] != 5 || lines[3] != 6 {
		t.Fatalf("ghArgs lines = %v, want 3, 4, 5 and 6", lines)
	}
}
