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
// gate (never `make fi`) with `make fix` as the fixer; done runs it in the
// agent's worktree and refuses with the red check's output (#212), and the
// branch lands once the agent runs the fixer.
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

	// Spawning installs the repo's hook for every worktree (#223), so the
	// agent's own commit is refused; the second skips hooks, as an agent
	// whose hooks don't run would (saddle's would deny it), and done's gate
	// catches it.
	w.Spawn("t1", "Loose", []string{"LOOSE"},
		fa.Write("LOOSE", "x\n"),
		fa.Run("git add -A && ! git commit -q -m loose"),
		fa.Step{Run: "git -c core.hooksPath=/dev/null commit -q -m loose", Unhooked: true},
		fa.Step{Done: "Adds LOOSE.", Optional: true}, // refused: the gate is red
		fa.Run("make fix"), fa.Commit("make fix"),
		fa.Done("Adds nothing loose."))
	w.WaitAgentLog("t1", "idle: script finished")
	log := w.AgentLog("t1")
	if !strings.Contains(log, "done refused") || !strings.Contains(log, "check: LOOSE found") {
		t.Fatalf("done didn't refuse with the gate's output:\n%s", log)
	}
	if strings.Contains(log, "No rule to make target") {
		t.Fatalf("the gate ran a target the Makefile lacks:\n%s", log)
	}
	w.WaitTask("t1", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	if r := w.MustSaddle("land"); w.Task("t1").Status != "landed" {
		t.Fatalf("t1 didn't land after make fix: %s", r)
	}
	if got := w.Git(w.Repo, "ls-tree", "--name-only", "saddle/integration"); strings.Contains(got, "LOOSE") {
		t.Fatalf("integration holds LOOSE:\n%s", got)
	}
}
