package guard

import (
	"strings"
	"testing"
)

func root(t *testing.T) string {
	t.Helper()
	r, err := ModuleRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestSizeBudget fails when v2 outgrows its budget (ADR 0013).
func TestSizeBudget(t *testing.T) {
	perDir, total, err := Lines(root(t))
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Fatal("counted no Go lines: the walk is broken")
	}
	if total > SizeBudget {
		t.Fatal(SizeReport(perDir, total, SizeBudget))
	}
}

func TestSizeBudgetCatchesATreeOverBudget(t *testing.T) {
	perDir, total, err := Lines("testdata/oversize")
	if err != nil {
		t.Fatal(err)
	}
	// big.go has 5 lines; big_test.go and testdata are not counted.
	if total != 5 || perDir["pkg"] != 5 {
		t.Fatalf("total %d, perDir %v; want 5 lines in pkg", total, perDir)
	}
	msg := SizeReport(perDir, total, 4)
	for _, want := range []string{"5 non-test Go lines", "budget is 4", "pkg"} {
		if !strings.Contains(msg, want) {
			t.Errorf("report %q lacks %q", msg, want)
		}
	}
}

// TestOnlyStoreWritesState fails when a package outside MayWriteFiles
// creates or changes a file: state belongs in the database (rule 1).
func TestOnlyStoreWritesState(t *testing.T) {
	vs, err := FileWrites(root(t), MayWriteFiles)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range vs {
		t.Errorf("%s: only %s may write files; keep state in the database through store", v, allowedNames())
	}
}

func allowedNames() string {
	var names []string
	for n := range MayWriteFiles {
		names = append(names, n)
	}
	return strings.Join(names, ", ")
}

func TestOnlyStoreWritesStateCatchesAWrite(t *testing.T) {
	vs, err := FileWrites("testdata/writes", map[string]string{"store": "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, v := range vs {
		got[v.String()] = true
	}
	want := []string{
		"internal/bad/bad.go:11: os.WriteFile",
		"internal/bad/bad.go:12: os.MkdirAll",
		"internal/bad/bad.go:13: io/ioutil.WriteFile",
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("missing violation %q; got %v", w, vs)
		}
	}
	if len(vs) != len(want) {
		t.Errorf("got %d violations, want %d: %v (reads and the store package must not count)", len(vs), len(want), vs)
	}
}
