package gitx_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/gitx/symbols"
)

func parseSpan(path string, src []byte) (gitx.Span, error) {
	syms, errs, err := symbols.Default().Parse(path, src)
	return gitx.Span{Symbols: syms, Errors: errs}, err
}

func mustChangeSet(t *testing.T, base, next string) gitx.ChangeSet {
	t.Helper()
	cs, err := gitx.ChangeSetFrom("p.go", []byte(base), []byte(next), parseSpan)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func TestConflicts(t *testing.T) {
	far := "package p\nfunc A() { return 1 }\n" + strings.Repeat("\n", 10) + "func B() { return 2 }\n"
	t.Run("different functions far apart", func(t *testing.T) {
		a := mustChangeSet(t, far, strings.Replace(far, "return 1", "return 9", 1))
		b := mustChangeSet(t, far, strings.Replace(far, "return 2", "return 8", 1))
		if why, conflict := gitx.Conflicts(a, b, 2); conflict {
			t.Fatalf("conflict %q, sets %+v and %+v", why, a, b)
		}
	})

	t.Run("same function", func(t *testing.T) {
		base := "package p\nfunc A() { return 1 }\n\nfunc B() { return 2 }\n"
		a := mustChangeSet(t, base, strings.Replace(base, "return 1", "return 9", 1))
		b := mustChangeSet(t, base, strings.Replace(base, "return 1", "return 8", 1))
		why, conflict := gitx.Conflicts(a, b, 2)
		if !conflict || why != "symbol func A" {
			t.Fatalf("why %q conflict %v, set %+v", why, conflict, a)
		}
	})

	t.Run("appends at EOF", func(t *testing.T) {
		base := "package p\nfunc A() { return 1 }\n"
		a := mustChangeSet(t, base, base+"func C() { return 1 }\n")
		b := mustChangeSet(t, base, base+"func D() { return 2 }\n")
		why, conflict := gitx.Conflicts(a, b, 2)
		if !conflict || why != "edits within 2 lines" {
			t.Fatalf("why %q conflict %v\nA %+v\nB %+v", why, conflict, a, b)
		}
	})

	t.Run("edits one line apart", func(t *testing.T) {
		base := "package p\nfunc A() { return 1 }\nfunc B() { return 2 }\n"
		a := mustChangeSet(t, base, strings.Replace(base, "return 1", "return 9", 1))
		b := mustChangeSet(t, base, strings.Replace(base, "return 2", "return 8", 1))
		if !reflect.DeepEqual(a.Ranges, []gitx.LineRange{{Start: 2, End: 2}}) || !reflect.DeepEqual(b.Ranges, []gitx.LineRange{{Start: 3, End: 3}}) {
			t.Fatalf("ranges %v and %v", a.Ranges, b.Ranges)
		}
		why, conflict := gitx.Conflicts(a, b, 2)
		if !conflict || why != "edits within 2 lines" {
			t.Fatalf("why %q conflict %v", why, conflict)
		}
	})

	t.Run("edits three lines apart", func(t *testing.T) {
		base := "package p\nfunc A() { return 1 }\n\n\nfunc B() { return 2 }\n"
		a := mustChangeSet(t, base, strings.Replace(base, "return 1", "return 9", 1))
		b := mustChangeSet(t, base, strings.Replace(base, "return 2", "return 8", 1))
		if !reflect.DeepEqual(a.Ranges, []gitx.LineRange{{Start: 2, End: 2}}) || !reflect.DeepEqual(b.Ranges, []gitx.LineRange{{Start: 5, End: 5}}) {
			t.Fatalf("ranges %v and %v", a.Ranges, b.Ranges)
		}
		if why, conflict := gitx.Conflicts(a, b, 2); conflict {
			t.Fatalf("conflict %q", why)
		}
	})

	t.Run("two new functions same name", func(t *testing.T) {
		base := "package p\nfunc A() { return 1 }\n"
		a := mustChangeSet(t, base, base+"func Foo() { return 1 }\n")
		b := mustChangeSet(t, base, base+"func Foo() { return 2 }\n")
		why, conflict := gitx.Conflicts(a, b, 2)
		if !conflict || why != "symbol func Foo" {
			t.Fatalf("why %q conflict %v\nA %+v\nB %+v", why, conflict, a, b)
		}
	})

	t.Run("same name different kind", func(t *testing.T) {
		a := gitx.ChangeSet{Symbols: []gitx.Symbol{{Name: "Foo", Kind: "func"}}}
		b := gitx.ChangeSet{Symbols: []gitx.Symbol{{Name: "Foo", Kind: "type"}}}
		if why, conflict := gitx.Conflicts(a, b, 2); conflict {
			t.Fatalf("conflict %q", why)
		}
	})
}

func TestWorktreeChangeSet(t *testing.T) {
	isolate(t)
	root := t.TempDir()
	run(t, root, "init", "-q", "-b", "integration")
	src := "package p\n\nfunc A() int {\n\treturn 1\n}\n\nfunc B() int {\n\treturn 2\n}\n"
	write(t, root, "p.go", src)
	run(t, root, "add", "p.go")
	run(t, root, "commit", "-q", "-m", "init")
	wt := filepath.Join(t.TempDir(), "wt")
	run(t, root, "worktree", "add", "-q", "-b", "task", wt, "integration")

	cs, blob, err := gitx.WorktreeChangeSet(wt, "integration", "p.go", parseSpan)
	if err != nil {
		t.Fatal(err)
	}
	wantBlob := run(t, wt, "rev-parse", "integration:p.go")
	if blob != wantBlob {
		t.Fatalf("base blob %s, want %s", blob, wantBlob)
	}
	if len(cs.Ranges) != 0 || len(cs.Symbols) != 0 {
		t.Fatalf("unchanged set %+v", cs)
	}

	write(t, wt, "p.go", strings.Replace(src, "return 1", "return 9", 1))
	cs, blob, err = gitx.WorktreeChangeSet(wt, "integration", "p.go", parseSpan)
	if err != nil {
		t.Fatal(err)
	}
	if blob != wantBlob {
		t.Fatalf("dirty edit moved the base blob to %s", blob)
	}
	if len(cs.Symbols) != 1 || cs.Symbols[0].Name != "A" || cs.Symbols[0].Kind != "func" {
		t.Fatalf("edited symbols %+v", cs.Symbols)
	}

	write(t, wt, "q.go", "package p\n\nfunc Q() {}\n")
	cs, blob, err = gitx.WorktreeChangeSet(wt, "integration", "q.go", parseSpan)
	if err != nil {
		t.Fatal(err)
	}
	if blob != "" {
		t.Fatalf("new file base blob %q", blob)
	}
	if len(cs.Symbols) != 1 || cs.Symbols[0].Name != "Q" {
		t.Fatalf("new file symbols %+v", cs.Symbols)
	}

	if err := os.Remove(filepath.Join(wt, "p.go")); err != nil {
		t.Fatal(err)
	}
	cs, blob, err = gitx.WorktreeChangeSet(wt, "integration", "p.go", parseSpan)
	if err != nil {
		t.Fatal(err)
	}
	if blob != wantBlob {
		t.Fatalf("deleted file base blob %s", blob)
	}
	var names []string
	for _, s := range cs.Symbols {
		names = append(names, s.Kind+" "+s.Name)
	}
	if !reflect.DeepEqual(names, []string{"func A", "func B"}) {
		t.Fatalf("deleted symbols %v", names)
	}

	if _, _, err := gitx.WorktreeChangeSet(wt, "integration", "../outside.go", parseSpan); err == nil {
		t.Fatal("path escape was accepted")
	}
}
