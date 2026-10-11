package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// #270: a repo hook that runs its check through make's variable, "${MAKE}"
// check or $(MAKE) check (recipe style; here MAKE prints the make to run,
// so the hook works when the test commits), is detected as
// make check. With [test] cmd = make check, a landing runs it once and the
// pre-publish gate lists it once.
func TestLintGateDetectsMakeVariableHookForms(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	for _, c := range []struct{ name, hook string }{
		{"braced", "#!/bin/sh\nMAKE=make\n\"${MAKE}\" check\n"},
		{"parens", "#!/bin/sh\nMAKE() { echo make; }\n$(MAKE) check\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a := trainSetup(t)
			count := filepath.Join(t.TempDir(), "count")
			write(t, a.Root, "Makefile", "check:\n\techo x >> "+count+"\n")
			write(t, a.Root, "git/hooks/pre-commit", c.hook)
			commitAll(t, a.Root, "repo gate")
			a.Cfg.Test.Cmd = "make check"
			if g := a.LintGate(); g.Cmd != "make check" {
				t.Fatalf("gate = %+v, want make check", g)
			}
			if cs := a.GateChecks(false); len(cs) != 1 {
				t.Fatalf("pre-publish checks = %+v, want make check once", cs)
			}
			queueTask(t, a, "t1", "once", map[string]string{"x.txt": "x\n"})
			before, _ := os.ReadFile(count)
			rs, err := a.Land()
			must(t, err)
			if len(rs) != 1 || rs[0].State != store.TrainOK {
				t.Fatalf("results = %+v", rs)
			}
			b, err := os.ReadFile(count)
			must(t, err)
			if n := strings.Count(string(b), "x") - strings.Count(string(before), "x"); n != 1 {
				t.Fatalf("the train ran make check %d times, want 1", n)
			}
		})
	}
}
