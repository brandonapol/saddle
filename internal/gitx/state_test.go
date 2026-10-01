package gitx

import (
	"reflect"
	"testing"
)

func TestParseStatus(t *testing.T) {
	out := "# branch.oid abc123\x00# branch.head feature\x00" +
		"1 .M N... 100644 100644 100644 h1 h2 a b.go\x00" +
		"2 R. N... 100644 100644 100644 h1 h2 R100 new name.go\x00old.go\x00" +
		"u UU N... 100644 100644 100644 100644 h1 h2 h3 conf.go\x00" +
		"? new/file.txt\x00"
	head, branch, files, renames := parseStatus(out)
	if head != "abc123" || branch != "feature" {
		t.Fatalf("head=%q branch=%q", head, branch)
	}
	var codes []string
	for _, f := range files {
		codes = append(codes, f.Code()+" "+f.Path)
	}
	want := []string{".M a b.go", "UU conf.go", "R. new name.go", "?? new/file.txt"}
	if !reflect.DeepEqual(codes, want) {
		t.Fatalf("files = %q, want %q", codes, want)
	}
	if !reflect.DeepEqual(renames, []PathRename{{Old: "old.go", New: "new name.go"}}) {
		t.Fatalf("renames = %+v", renames)
	}
	if !files[1].Conflict || files[0].Staged() || !files[2].Staged() {
		t.Fatalf("flags wrong: %+v", files)
	}

	head, branch, _, _ = parseStatus("# branch.oid (initial)\x00# branch.head (detached)")
	if head != "" || branch != "" {
		t.Fatalf("initial/detached: head=%q branch=%q", head, branch)
	}
}

func TestParseUnifiedZero(t *testing.T) {
	diff := `diff --git a/x.go b/x.go
index 1..2 100644
--- a/x.go
+++ b/x.go
@@ -3 +3 @@ func A() {
-a
+b
@@ -10,2 +9,0 @@ func B() {
-c
-d
@@ -20,0 +19,3 @@
+e
diff --git a/old.go b/new.go
similarity index 90%
rename from old.go
rename to new.go
--- a/old.go
+++ b/new.go
@@ -1 +1 @@
-x
+y
diff --git a/gone.go b/gone.go
deleted file mode 100644
--- a/gone.go
+++ /dev/null
@@ -1,4 +0,0 @@
`
	got := parseUnifiedZero(diff)
	want := []FileHunks{
		{OldPath: "x.go", NewPath: "x.go", Old: []LineRange{{3, 3}, {10, 11}}, New: []LineRange{{3, 3}, {19, 21}}},
		{OldPath: "old.go", NewPath: "new.go", Old: []LineRange{{1, 1}}, New: []LineRange{{1, 1}}},
		{OldPath: "gone.go", Old: []LineRange{{1, 4}}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %+v\nwant %+v", got, want)
	}
}

func TestEnclosingPicksInnermost(t *testing.T) {
	syms := []Symbol{
		{Name: "C", StartLine: 1, EndLine: 10},
		{Name: "C.m", StartLine: 3, EndLine: 5},
		{Name: "f", StartLine: 12, EndLine: 14},
	}
	got := enclosing(syms, []LineRange{{4, 4}, {2, 2}, {11, 11}, {14, 30}})
	var names []string
	for _, s := range got {
		names = append(names, s.Name)
	}
	if !reflect.DeepEqual(names, []string{"C.m", "C", "f"}) {
		t.Fatalf("enclosing = %v", names)
	}
}

func TestDiffStates(t *testing.T) {
	sym := TouchedSymbol{Path: "a.go", Symbol: Symbol{Name: "A", Kind: "func"}}
	prev := State{
		Head:    "h1",
		Files:   []FileStatus{{Path: "a.go", Index: '.', Worktree: 'M'}, {Path: "b.go", Index: '.', Worktree: 'M'}},
		Symbols: []TouchedSymbol{sym},
	}
	next := State{
		Task:    "t1",
		Head:    "h2",
		Files:   []FileStatus{{Path: "a.go", Index: 'M', Worktree: '.'}},
		Renames: []PathRename{{Old: "x", New: "y"}},
	}
	ds := diffStates(prev, next, func(a, b string) ([]string, bool) { return []string{"h2"}, true })
	var kinds []string
	for _, d := range ds {
		if d.From().Task != "t1" {
			t.Fatalf("task = %q", d.From().Task)
		}
		switch d := d.(type) {
		case Committed:
			kinds = append(kinds, "commit "+d.Old+".."+d.New)
		case RenameDetected:
			kinds = append(kinds, "rename "+d.Old+"->"+d.New)
		case FileDirty:
			if d.Clean {
				kinds = append(kinds, "clean "+d.Path)
			} else {
				kinds = append(kinds, "dirty "+d.Path+" "+d.Status.Code())
			}
		case SymbolTouched:
			kinds = append(kinds, "symbol "+d.Name+" cleared="+map[bool]string{true: "y", false: "n"}[d.Cleared])
		}
	}
	want := []string{"commit h1..h2", "rename x->y", "dirty a.go M.", "clean b.go", "symbol A cleared=y"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("deltas = %q, want %q", kinds, want)
	}

	ds = diffStates(State{Head: "h1"}, State{Head: "h3"}, func(a, b string) ([]string, bool) { return nil, false })
	if len(ds) != 1 {
		t.Fatalf("want one Rebased, got %+v", ds)
	}
	if _, ok := ds[0].(Rebased); !ok {
		t.Fatalf("want Rebased, got %T", ds[0])
	}
}
