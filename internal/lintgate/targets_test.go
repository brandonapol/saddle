package lintgate

import (
	"slices"
	"testing"
)

// #228: quark's hook ran `make` on a line of its own inside an if block.
// The make regex matched across the newline and took the `fi` terminator as
// the target, so done and the train ran `make fi` and failed on every land.
func TestDetectHookMakeFollowedByFi(t *testing.T) {
	cases := []struct {
		name, hook, want string
	}{
		{"bare make then fi", "#!/bin/sh\nif [ -f Makefile ]; then\n  make\nfi\n", "hook"},
		{"make with flags then fi", "#!/bin/sh\nif true; then\n  make -s\nfi\n", "hook"},
		{"make check inside if", "#!/bin/sh\nif [ -f Makefile ]; then\n  make check\nfi\n", "make check"},
		{"make then done", "#!/bin/sh\nfor d in a b; do\n  make\ndone\n", "hook"},
		{"skip clause before make", "#!/bin/sh\nif git diff --cached --quiet; then exit 0; fi\nmake -s check\n", "make check"},
		{"command -v make guard", "#!/bin/sh\ncommand -v make >/dev/null || exit 0\nmake lint\n", "make lint"},
		{"make && fix then check", "#!/bin/sh\nmake fix && git add -u\nmake check\n", "make check"},
		{"make target not in Makefile", "#!/bin/sh\nmake verify\n", "hook"},
		{"make -C elsewhere", "#!/bin/sh\nmake -C tools check-tools\n", "hook"},
		{"make -j4 check", "#!/bin/sh\nmake -j4 check\n", "make check"},
		{"make check/format", "#!/bin/sh\nexec make check/format\n", "make check/format"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRepo(t)
			r.write("Makefile", "check: lint\n\techo ok\nlint:\n\ttrue\nfix:\n\ttrue\ncheck/format: ## Check formatting\n\ttrue\n")
			p := r.write(".git/hooks/pre-commit", c.hook)
			want := c.want
			if want == "hook" {
				want = shellQuote(p)
			}
			g := r.detect()
			if g.Cmd != want || g.Kind != KindHook {
				t.Fatalf("Cmd = %q, want %q (%+v)", g.Cmd, want, g)
			}
		})
	}
}

// With no Makefile to check against, a hook's make target is taken as is,
// but never a shell keyword.
func TestDetectHookMakeWithoutMakefile(t *testing.T) {
	r := newRepo(t)
	r.write(".git/hooks/pre-commit", "#!/bin/sh\nexec make -s check\n")
	if g := r.detect(); g.Cmd != "make check" {
		t.Fatalf("%+v", g)
	}
	p := r.write(".git/hooks/pre-commit", "#!/bin/sh\nif true; then make\nfi\n")
	if g := r.detect(); g.Cmd != shellQuote(p) {
		t.Fatalf("keyword: %+v", g)
	}
}

func TestMakeTargets(t *testing.T) {
	cases := []struct {
		name, makefile string
		want           []string
	}{
		{"plain", "check:\n\ttrue\n", []string{"check"}},
		{"fix is not fi", "fix:\n\ttrue\n", []string{"fix"}},
		{"prerequisites", "check: lint test\n\ttrue\n", []string{"check"}},
		{"help comment", "check: ## Run every check\n\ttrue\nfix: lint ## Fix what can be fixed\n", []string{"check", "fix"}},
		{"slash and dash", "check/format:\n\ttrue\nlint-go lint/py: deps\n", []string{"check/format", "lint-go", "lint/py"}},
		{"double colon", "fix:: a\n\ttrue\n", []string{"fix"}},
		{"phony only", ".PHONY: check fix\n", nil},
		{"phony and rule", ".PHONY: check\ncheck:\n\ttrue\n", []string{"check"}},
		{"simple variable", "check := yes\nX ::= a:b\nY = c:d\nZ ?= e:f\nW += g:h\nV != echo i:j\n", nil},
		{"target-specific variable", "check: GOFLAGS = -v\ncheck: lint\n", []string{"check"}},
		{"target-specific only", "check: GOFLAGS := -v\n", nil},
		{"recipe lines", "all:\n\tcheck: not a rule\n", []string{"all"}},
		{"pattern rule", "%.o: %.c\n\tcc $<\n", nil},
		{"comment", "# check: commented out\n", nil},
		{"define block", "define T\ncheck: inside\nendef\n", nil},
		{"order-only and semicolon", "check: a | b\nlint: ; golangci-lint run\n", []string{"check", "lint"}},
		{"colon in prerequisite", "check: C:/x\n", []string{"check"}},
		{"export", "export check := 1\n", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseTargets(c.makefile)
			if !slices.Equal(got, c.want) {
				t.Fatalf("targets = %q, want %q", got, c.want)
			}
		})
	}
}

func TestHasTargetFollowsInclude(t *testing.T) {
	r := newRepo(t)
	r.write("Makefile", "include mk/check.mk\n-include missing.mk\nfix:\n\ttrue\n")
	r.write("mk/check.mk", "check: ## from an include\n\ttrue\n")
	if !hasTarget(r.root, "check") || !hasTarget(r.root, "fix") || hasTarget(r.root, "fi") {
		t.Fatalf("targets with includes: %q", makeTargets(r.root))
	}
	if g := r.detect(); g.Cmd != "make check" || g.Fix != "make fix" {
		t.Fatalf("%+v", g)
	}
}

func TestMakeLine(t *testing.T) {
	cases := map[string][]string{
		"make check":             {"check"},
		"exec make -s check":     {"check"},
		"make -k -j8 lint test":  {"lint"},
		"make":                   nil,
		"make >/dev/null":        nil,
		"cmake --build .":        nil,
		"gmake check":            nil,
		"$(MAKE) check":          nil,
		"make lint && make test": {"lint", "test"},
		"make -C sub check":      nil,
		"make -f other.mk check": nil,
		`make "check"`:           nil,
	}
	for line, want := range cases {
		if got := makeTargetsIn(line); !slices.Equal(got, want) {
			t.Errorf("%q: %q, want %q", line, got, want)
		}
	}
}
