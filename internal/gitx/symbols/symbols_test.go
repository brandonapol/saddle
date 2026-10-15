package symbols

import (
	"reflect"
	"testing"

	"github.com/brandonapol/saddle/internal/gitx"
)

func names(syms []gitx.Symbol) []string {
	var out []string
	for _, s := range syms {
		out = append(out, s.Kind+" "+s.Name)
	}
	return out
}

func TestGoSymbols(t *testing.T) {
	src := `package p

type T struct {
	A int
}

func (t *T) M() int {
	type local int
	return 1
}

func (g G[K]) N() {}

func F() {}

var x, y = 1, 2

const C = 3

type (
	U int
	V = string
)
`
	syms, err := Default().Symbols("p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"type T", "method T.M", "method G.N", "func F", "var x,y", "const C", "type U", "type V"}
	if got := names(syms); !reflect.DeepEqual(got, want) {
		t.Fatalf("symbols = %v, want %v", got, want)
	}
	if s := syms[1]; s.StartLine != 7 || s.EndLine != 10 {
		t.Fatalf("T.M lines = %d-%d, want 7-10", s.StartLine, s.EndLine)
	}
	if s := syms[0]; s.StartLine != 3 || s.EndLine != 5 {
		t.Fatalf("T lines = %d-%d, want 3-5", s.StartLine, s.EndLine)
	}
}

func TestPythonSymbols(t *testing.T) {
	src := "class A:\n    def m(self):\n        pass\n\n\ndef f():\n    def inner():\n        pass\n"
	syms, err := Default().Symbols("a.py", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"class A", "func A.m", "func f", "func f.inner"}
	if got := names(syms); !reflect.DeepEqual(got, want) {
		t.Fatalf("symbols = %v, want %v", got, want)
	}
}

func TestBrokenSourceStillParses(t *testing.T) {
	src := "package p\n\nfunc A() {\n\tx := \n}\n\nfunc B() {}\n"
	syms, err := Default().Symbols("p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range syms {
		found = found || s.Name == "B"
	}
	if !found {
		t.Fatalf("B not found in %v", names(syms))
	}
}

func TestSigTextIsTheDeclaration(t *testing.T) {
	src := "package store\n\nfunc (s *Store) Claim(path string) error {\n\treturn nil\n}\n"
	syms, err := Default().Symbols("store.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(syms) != 1 {
		t.Fatalf("symbols = %v", names(syms))
	}
	if syms[0].SigText != "func (s *Store) Claim(path string) error" {
		t.Fatalf("SigText = %q", syms[0].SigText)
	}
	body := "package store\n\nfunc (s *Store) Claim(path string) error {\n\treturn errors.New(\"no\")\n}\n"
	again, err := Default().Symbols("store.go", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if again[0].SigText != syms[0].SigText || again[0].SigHash != syms[0].SigHash {
		t.Fatalf("body edit moved the signature: %q %s", again[0].SigText, again[0].SigHash)
	}
}

func TestUsesNamesAndSelectors(t *testing.T) {
	src := "package store\n\nfunc Use(s *Store) error {\n\treturn s.Claim(\"Claim\")\n}\n"
	uses, err := Default().Uses("store/use.go", []byte(src), map[int]bool{4: true})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, u := range uses {
		got[u.Qual+" "+u.Name] = true
	}
	if !got["s Claim"] || !got[" Claim"] {
		t.Fatalf("uses = %v", got)
	}
	// The string "Claim" is not an identifier, and line 3 is not added.
	for _, u := range uses {
		if u.Name == "Use" || u.Name == "Store" {
			t.Fatalf("use %q is not on the added line", u.Name)
		}
	}
}

func TestSupports(t *testing.T) {
	x := Default()
	if !x.Supports("a/b.go") || !x.Supports("x.py") || x.Supports("README.md") {
		t.Fatal("Supports by extension is wrong")
	}
	if x.Language("a/b.go") != "go" || x.Language("x.py") != "python" || x.Language("README.md") != "" {
		t.Fatal("Language by extension is wrong")
	}
}

// TestSigHash locks the one-line shapes a line-based check misreads: a body
// edit keeps the hash, a signature edit changes it. Whitespace is not part of
// the signature, so gofmt spacing is not a change either.
func TestSigHash(t *testing.T) {
	x := Default()
	hash := func(path, src string) string {
		t.Helper()
		syms, err := x.Symbols(path, []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		if len(syms) != 1 {
			t.Fatalf("%s %q: symbols %v, want one", path, src, names(syms))
		}
		if len(syms[0].SigHash) != 64 {
			t.Fatalf("SigHash = %q, want 64 hex chars", syms[0].SigHash)
		}
		return syms[0].SigHash
	}
	cases := []struct {
		name, path, a, b string
		same             bool
	}{
		{"func body", "a.go",
			"package p\nfunc f() int { return 1 }\n",
			"package p\nfunc f() int { return 2 }\n", true},
		{"func result", "a.go",
			"package p\nfunc f() int { return 1 }\n",
			"package p\nfunc f() int64 { return 1 }\n", false},
		{"func spacing", "a.go",
			"package p\nfunc f() int { return 1 }\n",
			"package p\nfunc  f()  int { return 2 }\n", true},
		{"var func body", "a.go",
			"package p\nvar h = func(x int) { return x }\n",
			"package p\nvar h = func(x int) { return x + 1 }\n", true},
		{"var value", "a.go",
			"package p\nvar h int = 1\n",
			"package p\nvar h int = 2\n", true},
		{"var type", "a.go",
			"package p\nvar h int = 1\n",
			"package p\nvar h int64 = 1\n", false},
		{"alias target", "a.go",
			"package p\ntype A = B\n",
			"package p\ntype A = C\n", false},
		{"alias spacing", "a.go",
			"package p\ntype A = B\n",
			"package p\ntype A=B\n", true},
		{"const value", "a.go",
			"package p\nconst c = 1\n",
			"package p\nconst c = 2\n", true},
		{"const typed value", "a.go",
			"package p\nconst c int = 1\n",
			"package p\nconst c int = 2\n", true},
		{"const typed type", "a.go",
			"package p\nconst c int = 1\n",
			"package p\nconst c int64 = 1\n", false},
		{"method body", "a.go",
			"package p\nfunc (t *T) M() {}\n",
			"package p\nfunc (t *T) M() { return }\n", true},
		{"method params", "a.go",
			"package p\nfunc (t *T) M() {}\n",
			"package p\nfunc (t *T) M(x int) {}\n", false},
		{"python body", "a.py",
			"def f(): return 1\n",
			"def f(): return 2\n", true},
		{"python params", "a.py",
			"def f(): return 1\n",
			"def f(x): return 1\n", false},
		{"class body", "a.py",
			"class A:\n    x = 1\n",
			"class A:\n    x = 2\n", true},
		{"class bases", "a.py",
			"class A:\n    pass\n",
			"class A(B):\n    pass\n", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ha, hb := hash(c.path, c.a), hash(c.path, c.b)
			if (ha == hb) != c.same {
				t.Fatalf("same=%v, hashes %s and %s", c.same, ha, hb)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	x := Default()
	clean := "package p\n\nfunc B() {}\n"
	syms, errs, err := x.Parse("p.go", []byte(clean))
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 0 {
		t.Fatalf("clean file errors = %v", errs)
	}
	if len(syms) != 1 || syms[0].Name != "B" {
		t.Fatalf("clean symbols = %v", names(syms))
	}

	// The value of `x :=` is missing. That missing node is the error range.
	broken := "package p\n\nfunc A() {\n\tx := \n}\n"
	_, errs, err = x.Parse("p.go", []byte(broken))
	if err != nil {
		t.Fatal(err)
	}
	want := []gitx.LineRange{{Start: 4, End: 4}}
	if !reflect.DeepEqual(errs, want) {
		t.Fatalf("errors = %v, want %v", errs, want)
	}
}
