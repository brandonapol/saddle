package symbols

import "testing"

// TestSupportedLanguagesLoad guards the build tags in the Makefile
// (grammar_subset + one tag per supported language). If a language is added to
// Default without its grammar_subset_<name> tag, its grammar is missing from
// the tagged binary and this fails under `make test`.
func TestSupportedLanguagesLoad(t *testing.T) {
	cases := map[string]string{
		"a.go": "package p\n\nfunc F() {}\n",
		"a.py": "def f():\n    pass\n",
	}
	x := Default()
	for path, src := range cases {
		syms, err := x.Symbols(path, []byte(src))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if len(syms) != 1 {
			t.Fatalf("%s: got %v, want one symbol", path, names(syms))
		}
	}
}
