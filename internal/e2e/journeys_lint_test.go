//go:build e2e

package e2e

import (
	"os/exec"
	"strings"
	"testing"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

// gofmtMakefile is a repo gate like quark's: make lint fails on unformatted
// Go, make fix formats it.
const gofmtMakefile = "lint:\n\t@out=\"$$(gofmt -l .)\"; if [ -n \"$$out\" ]; then echo \"gofmt needs: $$out\"; exit 1; fi\n\nfix:\n\tgofmt -w .\n"

// TestJourneyLintDoneRefusedThenLands (#212): an agent commits Go that gofmt
// rejects and calls done. done runs the repo's gate (detected from the
// Makefile) and refuses with gofmt's output; the agent runs the repo's
// fixer, commits and calls done again; the branch lands formatted.
func TestJourneyLintDoneRefusedThenLands(t *testing.T) {
	// App.Done lives in internal/app/app.go, held by t76; the call to
	// lintDone lands right after it. Until then done doesn't run the gate.
	t.Skip("needs App.Done to call a.lintDone (internal/app/app.go, after t76 lands)")
	if _, err := exec.LookPath("gofmt"); err != nil {
		t.Skip("gofmt not installed")
	}
	w := world(t, Options{})
	w.WriteFile("Makefile", gofmtMakefile)
	w.Git(w.Repo, "add", "Makefile")
	w.Git(w.Repo, "commit", "-q", "-m", "lint gate")
	w.Git(w.Repo, "push", "-q", "origin", "main")

	w.Spawn("t1", "Unformatted", []string{"main.go"},
		fa.Write("main.go", "package main\nfunc main(){}\n"),
		fa.Commit("main"),
		fa.Step{Done: "Adds main.", Optional: true}, // refused: gofmt is red
		fa.Run("make fix"),
		fa.Commit("gofmt"),
		fa.Done("Adds main, formatted."))

	w.WaitAgentLog("t1", "done refused")
	if log := w.AgentLog("t1"); !strings.Contains(log, "gofmt needs: main.go") || !strings.Contains(log, "make fix") {
		t.Fatalf("refusal lacks gofmt's output or the fixer:\n%s", log)
	}
	w.WaitTask("t1", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	if r := w.MustSaddle("land"); w.Task("t1").Status != "landed" {
		t.Fatalf("t1 didn't land: %s", r)
	}
	if got := w.Git(w.Repo, "show", "saddle/integration:main.go"); got != "package main\n\nfunc main() {}" {
		t.Fatalf("integration main.go = %q", got)
	}
}

// TestJourneyLintFailureReturnedToProducer (#212): t2's tree passes the
// repo's gate on its own, but not once rebased onto t1's landed work. The
// train runs lint.cmd after the tests on the rebased tree and hands the
// failure back to t2 with the output, like red tests; t2 fixes it and lands.
func TestJourneyLintFailureReturnedToProducer(t *testing.T) {
	lint := `if [ -e STRICT ] && grep -rlq --exclude-dir=.git LOOSE .; then echo "lint: LOOSE is not allowed under STRICT"; exit 1; fi`
	w := world(t, Options{Tables: "[train]\nno_auto_rebase = true\nlint.cmd = " + strqt(lint) + "\n"})
	w.Spawn("t1", "Strict", []string{"STRICT"}, fa.Write("STRICT", "on\n"), fa.Commit("strict"), fa.Done("Strict mode."))
	w.Spawn("t2", "Loose", []string{"loose.txt"},
		fa.Write("loose.txt", "LOOSE\n"), fa.Commit("loose"),
		fa.Wait("proceed-now"),
		fa.Done("Loose."),
		fa.Wait("passed its tests, but"),
		fa.Write("loose.txt", "tight\n"), fa.Commit("tighten"),
		fa.Done("Tight."))
	w.WaitTask("t1", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	if r := w.MustSaddle("land"); w.Task("t1").Status != "landed" {
		t.Fatalf("t1 didn't land: %s", r)
	}
	w.MustSaddle("message", "t2", "proceed-now")
	w.WaitTask("t2", "queued", func(v mcpserver.TaskView) bool { return v.Train == "queued" })

	r := w.Saddle("land")
	if v := w.Task("t2"); v.Status != "conflict" || strings.HasPrefix(v.Train, "landed") {
		t.Fatalf("t2 after a red lint = %+v\n%s", v, r)
	}
	w.WaitAgentLog("t2", "lint: LOOSE is not allowed under STRICT")
	w.WaitAgentLog("t2", "idle: script finished")
	w.WaitTask("t2", "queued again", func(v mcpserver.TaskView) bool { return v.Train == "queued" })
	if r := w.MustSaddle("land"); w.Task("t2").Status != "landed" {
		t.Fatalf("t2 didn't land after the fix: %s", r)
	}
}

// strqt quotes s as a TOML literal string.
func strqt(s string) string { return "'" + s + "'" }
