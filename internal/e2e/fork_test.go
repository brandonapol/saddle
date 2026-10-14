//go:build e2e

package e2e

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHarnessEveryTestForks: a journey that doesn't start with Fork runs in
// the parent process, serially (the suite got slow, #521), and its World's
// process env would leak into the parallel journeys' parent.
func TestHarnessEveryTestForks(t *testing.T) {
	if Fork(t) {
		return
	}
	var files []string
	for _, dir := range []string{".", "../../test/e2e"} {
		m, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
		must(t, err)
		files = append(files, m...)
	}
	if len(files) < 10 {
		t.Fatalf("found only %d test files: %v", len(files), files)
	}
	fset := token.NewFileSet()
	for _, p := range files {
		f, err := parser.ParseFile(fset, p, nil, 0)
		must(t, err)
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || !strings.HasPrefix(fn.Name.Name, "Test") || fn.Name.Name == "TestMain" {
				continue
			}
			if !startsWithFork(fset, fn) {
				t.Errorf("%s: %s doesn't start with `if Fork(t) { return }`", fset.Position(fn.Pos()), fn.Name.Name)
			}
		}
	}
}

func startsWithFork(fset *token.FileSet, fn *ast.FuncDecl) bool {
	if fn.Body == nil || len(fn.Body.List) == 0 {
		return false
	}
	var b bytes.Buffer
	if printer.Fprint(&b, fset, fn.Body.List[0]) != nil {
		return false
	}
	s := strings.Join(strings.Fields(b.String()), " ")
	return s == "if Fork(t) { return }" || s == "if harness.Fork(t) { return }"
}

// TestHarnessLoadScale: the budget grows with the load other processes put
// on the machine, not with the suite's own.
func TestHarnessLoadScale(t *testing.T) {
	if Fork(t) {
		return
	}
	const took = 100 * time.Second
	for _, c := range []struct {
		name string
		load float64
		own  time.Duration
		want float64
	}{
		{"unknown load", 0, 0, 1},
		{"only the suite's own load", 8, 800 * time.Second, 1},
		{"quiet", 0.5, 400 * time.Second, 1},
		{"others load every core", 16, 800 * time.Second, 2},
		{"others load half the cores", 12, 800 * time.Second, 1.5},
	} {
		if got := loadScale(c.load, c.own, took, 8); got != c.want {
			t.Errorf("%s: loadScale(%v, %s) = %v, want %v", c.name, c.load, c.own, got, c.want)
		}
	}
}

// TestHarnessReportListsSlowest: the report names the slowest journeys,
// slowest first, and the ones still running when the suite times out.
func TestHarnessReportListsSlowest(t *testing.T) {
	if Fork(t) {
		return
	}
	j := &timings{started: map[string]time.Time{}, took: map[string]time.Duration{}}
	for i := range 12 {
		j.end("TestJourney"+itoa(i), time.Duration(i)*time.Second)
	}
	j.end("TestJourney3", time.Second) // a faster -count rerun keeps the slowest
	j.start("TestJourneyHung", time.Now().Add(-time.Hour))
	var b bytes.Buffer
	j.report(&b)
	out := b.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1+slowest {
		t.Fatalf("report has %d lines, want a header and %d journeys:\n%s", len(lines), slowest, out)
	}
	for i, want := range []string{"12 journeys finished", "TestJourneyHung (still running)", "TestJourney11", "TestJourney10"} {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d = %q, want %q in it:\n%s", i, lines[i], want, out)
		}
	}
	for _, l := range lines {
		if strings.HasSuffix(l, " TestJourney1") || strings.HasSuffix(l, " TestJourney0") {
			t.Errorf("the report lists more than the %d slowest:\n%s", slowest, out)
		}
	}
}

// TestHarnessInProcessAppRunsSaddle: an App opened in the test process
// took the test binary for saddle (os.Executable), so agents it spawned ran
// `e2e.test hook` as their hooks: each started the whole suite and was
// killed at the hook timeout.
func TestHarnessInProcessAppRunsSaddle(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{})
	if a := w.App(); a.Bin != w.Bins.Saddle {
		t.Fatalf("World.App's saddle binary is %s, want %s", a.Bin, w.Bins.Saddle)
	}
}
