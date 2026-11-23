// Package guard holds the checks that keep v2 small and keep its state in
// one place (ADR 0013, rule 1). They run as tests in make check.
package guard

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// SizeBudget is the most non-test Go lines v2 may hold.
const SizeBudget = 20000

// MayWriteFiles lists the packages under internal/ that may create or change
// files, each with the reason. Every other package keeps its state in the
// database through store.
var MayWriteFiles = map[string]string{
	"store": "owns the database and the state directory",
	"turn":  "writes attempt logs and the files a driver asks for",
	"git":   "creates and removes worktrees",
	"guard": "reads the source tree; writes nothing",
}

// ModuleRoot walks up from dir to the directory that holds go.mod.
func ModuleRoot(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// goFiles calls fn for every non-test Go file under root, skipping testdata
// and hidden directories.
func goFiles(root string, fn func(path string) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path != root && (name == "testdata" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		return fn(path)
	})
}

// Lines counts non-test Go lines under root, per directory relative to root.
func Lines(root string) (perDir map[string]int, total int, err error) {
	perDir = map[string]int{}
	err = goFiles(root, func(path string) error {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		n := 0
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			n++
		}
		if err := sc.Err(); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		perDir[filepath.ToSlash(rel)] += n
		total += n
		return nil
	})
	return perDir, total, err
}

// SizeReport is the failure message for a tree over budget: the total and
// each directory, largest first.
func SizeReport(perDir map[string]int, total, budget int) string {
	dirs := make([]string, 0, len(perDir))
	for d := range perDir {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool {
		if perDir[dirs[i]] != perDir[dirs[j]] {
			return perDir[dirs[i]] > perDir[dirs[j]]
		}
		return dirs[i] < dirs[j]
	})
	var b strings.Builder
	fmt.Fprintf(&b, "v2 holds %d non-test Go lines; the budget is %d (ADR 0013). Per package:\n", total, budget)
	for _, d := range dirs {
		fmt.Fprintf(&b, "  %6d  %s\n", perDir[d], d)
	}
	return b.String()
}

// writers are the functions that create or change files, by import path.
var writers = map[string]map[string]bool{
	"os": {
		"WriteFile": true, "Create": true, "CreateTemp": true, "OpenFile": true,
		"Mkdir": true, "MkdirAll": true, "MkdirTemp": true, "Rename": true,
		"Remove": true, "RemoveAll": true, "Truncate": true, "Symlink": true,
		"Link": true, "Chmod": true, "CopyFS": true,
	},
	"io/ioutil": {"WriteFile": true, "TempFile": true, "TempDir": true},
}

// Violation is one call that writes a file from a package that may not.
type Violation struct {
	Pos  string // file:line, relative to the module root
	Call string // e.g. os.WriteFile
}

func (v Violation) String() string { return v.Pos + ": " + v.Call }

// FileWrites finds calls that create or change files in the packages under
// root/internal that are not in allowed.
func FileWrites(root string, allowed map[string]string) ([]Violation, error) {
	var out []Violation
	internal := filepath.Join(root, "internal")
	fset := token.NewFileSet()
	err := goFiles(internal, func(path string) error {
		rel, err := filepath.Rel(internal, path)
		if err != nil {
			return err
		}
		pkg := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
		if _, ok := allowed[pkg]; ok {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		names := map[string]string{} // local name -> import path
		for _, imp := range file.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil || writers[p] == nil {
				continue
			}
			name := filepath.Base(p)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			names[name] = p
		}
		if len(names) == 0 {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if p, ok := names[id.Name]; ok && writers[p][sel.Sel.Name] {
				pos := fset.Position(sel.Pos())
				relPos, _ := filepath.Rel(root, pos.Filename)
				out = append(out, Violation{
					Pos:  fmt.Sprintf("%s:%d", filepath.ToSlash(relPos), pos.Line),
					Call: p + "." + sel.Sel.Name,
				})
			}
			return true
		})
		return nil
	})
	return out, err
}
