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

// TestJourneyTrustPrompt (#215): in a fresh repo saddle asks whether you
// trust the folder before writing anything. Non-interactive init and up
// refuse; on a real terminal, no writes nothing and yes initializes and is
// remembered; a changed origin asks again; agents in worktrees are never
// asked, even after the user untrusts the repo.
func TestJourneyTrustPrompt(t *testing.T) {
	t.Setenv("SADDLE_TRUST", "")
	w := world(t, Options{NoInit: true})
	saddleDir := filepath.Join(w.Repo, ".saddle")
	nothingWritten := func(when string) {
		t.Helper()
		if _, err := os.Stat(saddleDir); !os.IsNotExist(err) {
			t.Fatalf("%s: .saddle exists: %v", when, err)
		}
		if _, err := os.Stat(filepath.Join(w.Repo, ".git", "hooks", "reference-transaction")); !os.IsNotExist(err) {
			t.Fatalf("%s: a git hook was installed: %v", when, err)
		}
	}

	// Without a terminal: the prompt text, a refusal naming --trust, nothing written.
	r := w.Saddle("init", "-q")
	if r.Code == 0 || !strings.Contains(r.Stdout, "Do you trust "+w.Repo) || !strings.Contains(r.Stdout, ".claude/settings.local.json") ||
		!strings.Contains(r.Stderr, "--trust") {
		t.Fatalf("non-interactive init: %s", r)
	}
	nothingWritten("non-interactive init")
	r = w.Saddle("up", "--skip-doctor")
	if r.Code == 0 || !strings.Contains(r.Stderr, "saddle trust") {
		t.Fatalf("untrusted up: %s", r)
	}
	nothingWritten("untrusted up")
	if r := w.MustSaddle("trust", "status"); !strings.Contains(r.Stdout, "not trusted") {
		t.Fatalf("trust status: %s", r)
	}

	// On a terminal: the banner, then the prompt. No exits and writes nothing.
	ask := func(name, answer string) *TUI {
		t.Helper()
		cmd := shq(w.Bins.Saddle) + ` init; echo "[saddle init exited $?]"; exec cat`
		// Wide enough that "initialized <repo>/.saddle" never wraps, however
		// long TMPDIR is (the train's gate runs under the user cache dir).
		must(t, w.Tmux.NewSession(name, max(120, len(w.Repo)+80), 50, w.Repo, cmd))
		u := &TUI{w: w, Target: name + ":0"}
		u.WaitScreen("Howdy", "1. Yes, trust this folder and continue", "2. No, exit (nothing written)", "Choose 1 or 2")
		u.Type(answer)
		u.Keys("Enter")
		return u
	}
	ask("e2e-trust-no", "2").WaitScreen("[saddle init exited 1]")
	nothingWritten("declined init")
	ask("e2e-trust-yes", "1").WaitScreen("initialized "+w.Repo+"/.saddle", "[saddle init exited 0]")
	w.WriteConfig(Options{})

	// Remembered, with the user's-only mode; doctor reports it; init doesn't ask again.
	fi, err := os.Stat(filepath.Join(w.Home, ".config", "saddle", "trust.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("trust.json: %v %v", fi, err)
	}
	if r := w.MustSaddle("init", "-q"); strings.Contains(r.Stdout, "Do you trust") {
		t.Fatalf("remembered decision re-asked: %s", r)
	}
	r = w.Saddle("doctor", "--json")
	if c := doctorChecks(t, r.Stdout)["trust"]; c.Status != "ok" {
		t.Fatalf("doctor trust check after yes: %+v\n%s", c, r)
	}

	// A changed origin asks again.
	other := filepath.Join(w.Root, "other.git")
	w.Git(w.Root, "init", "-q", "--bare", "-b", "main", other)
	w.Git(w.Repo, "remote", "set-url", "origin", other)
	if r := w.Saddle("up", "--skip-doctor"); r.Code == 0 || !strings.Contains(r.Stderr, "saddle trust") {
		t.Fatalf("up after the origin changed: %s", r)
	}
	if r := w.MustSaddle("trust", "status"); !strings.Contains(r.Stdout, "origin changed") {
		t.Fatalf("trust status after the origin changed: %s", r)
	}
	r = w.Saddle("doctor", "--json")
	if c := doctorChecks(t, r.Stdout)["trust"]; c.Status != "fail" || !strings.Contains(c.Fix, "saddle trust") {
		t.Fatalf("doctor trust check after the origin changed: %+v", c)
	}
	w.Git(w.Repo, "remote", "set-url", "origin", w.Origin)
	w.MustSaddle("trust", "status")

	// Agents run under a trusted parent: untrusting the repo doesn't block
	// an agent in its worktree, which can still run saddle there.
	w.MustSaddle("untrust")
	w.Spawn("t1", "Agent work", []string{"alpha/**"},
		fa.Run("saddle init -q && saddle trust status"),
		fa.Write("alpha/work.txt", "alpha\n"), fa.Commit("work in alpha"), fa.Done("Adds alpha/work.txt."))
	w.WaitTask("t1", "queued in the train", func(v mcpserver.TaskView) bool { return v.Status == "done" && v.Train == "queued" })
	if log := w.AgentLog("t1"); !strings.Contains(log, "this run is trusted anyway") {
		t.Fatalf("agent's trust status:\n%s", log)
	}
}
