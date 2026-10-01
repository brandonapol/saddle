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

func TestSupports(t *testing.T) {
	x := Default()
	if !x.Supports("a/b.go") || !x.Supports("x.py") || x.Supports("README.md") {
		t.Fatal("Supports by extension is wrong")
	}
}
