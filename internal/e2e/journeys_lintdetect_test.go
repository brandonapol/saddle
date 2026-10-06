//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// checkFixMakefile has a check that fails while a LOOSE file exists and a
// fixer that removes it. check is the first target, so a bare `make` runs it.
const checkFixMakefile = ".PHONY: check fix\n" +
	"check: ## Run the repo's checks\n\t@if [ -e LOOSE ]; then echo \"check: LOOSE found\"; exit 1; fi\n\n" +
	"fix: ## Fix what can be fixed\n\trm -f LOOSE\n"

// quarkHook is shaped like the hook that was detected as `make fi` (#228):
// a bare make inside an if block, its `fi` on the next line.
const quarkHook = "#!/bin/sh\n# Run the repo's checks before every commit.\nif [ -z \"$SKIP_CHECK\" ]; then\n  make -s\nfi\n"

// TestJourneyLintDetectMakeFi (#228): the repo ships a pre-commit hook that
// runs a bare `make` inside an if block, and a Makefile with check and fix
// targets. saddle used to detect the gate as `make fi` and fail every land
// with "No rule to make target". Now doctor reports the hook itself as the
// gate (never `make fi`) with `make fix` as the fixer; the train runs it on
// the rebased tree, returns the red check to the producer with its output,
// and lands the branch once the agent runs the fixer.
func TestJourneyLintDetectMakeFi(t *testing.T) {
	w := world(t, Options{Tables: "[train]\nno_auto_rebase = true\n"})
	w.WriteFile("Makefile", checkFixMakefile)
	w.WriteFile("git/hooks/pre-commit", quarkHook)
	must(t, os.Chmod(filepath.Join(w.Repo, "git/hooks/pre-commit"), 0o755))
	w.Git(w.Repo, "add", "Makefile", "git/hooks/pre-commit")
	w.Git(w.Repo, "commit", "-q", "-m", "repo gate")
	w.Git(w.Repo, "push", "-q", "origin", "main")

	doc := w.Saddle("doctor")
	out := doc.Stdout + doc.Stderr
	if strings.Contains(out, "make fi`") || !strings.Contains(out, "git/hooks/pre-commit") || !strings.Contains(out, "fix: make fix") {
		t.Fatalf("doctor's lint gate:\n%s", out)
	}

	w.Spawn("t1", "Loose", []string{"LOOSE"},
		fa.Write("LOOSE", "x\n"), fa.Commit("loose"),
		fa.Done("Adds LOOSE."),
		fa.Wait("passed its tests, but"),
		fa.Run("make fix"), fa.Commit("make fix"),
		fa.Done("Adds nothing loose."))
	w.WaitTask("t1", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })

	r := w.Saddle("land")
	if v := w.Task("t1"); v.Status != "conflict" {
		t.Fatalf("t1 after a red gate = %+v\n%s", v, r)
	}
	w.WaitAgentLog("t1", "check: LOOSE found")
	if log := w.AgentLog("t1"); strings.Contains(log, "No rule to make target") {
		t.Fatalf("the gate ran a target the Makefile lacks:\n%s", log)
	}
	w.WaitAgentLog("t1", "idle: script finished")
	w.WaitTask("t1", "queued again", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	if r := w.MustSaddle("land"); w.Task("t1").Status != "landed" {
		t.Fatalf("t1 didn't land after make fix: %s", r)
	}
	if got := w.Git(w.Repo, "ls-tree", "--name-only", "saddle/integration"); strings.Contains(got, "LOOSE") {
		t.Fatalf("integration holds LOOSE:\n%s", got)
	}
}
