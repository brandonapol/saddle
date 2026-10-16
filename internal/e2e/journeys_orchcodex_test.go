//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

// codexOnly turns w into a machine with Codex and no Claude Code (#668):
// the claude wrapper [claude] cmd names is removed, and fakecodex is
// installed as codex on PATH with a login. It returns codex's home.
func codexOnly(t *testing.T, w *World) string {
	t.Helper()
	root, err := moduleRoot()
	must(t, err)
	cmd := exec.Command("go", "build", "-o", filepath.Join(w.Bin, "codex"), "./internal/agent/agenttest/fakecodex/cmd/codex")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), goEnv(t)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fakecodex: %v\n%s", err, out)
	}
	must(t, os.Remove(filepath.Join(w.Bin, "claude")))
	home := filepath.Join(w.Home, ".codex")
	must(t, os.MkdirAll(home, 0o755))
	must(t, os.WriteFile(filepath.Join(home, "auth.json"), []byte("{}"), 0o600))
	return home
}

// TestJourneyCodexOrchestrator (#668): with claude absent and codex present,
// saddle init picks codex for the workers and the orchestrator, doctor is
// clean for the orchestrator, and saddle up starts a codex orchestrator. It
// calls saddle's tools over the MCP server saddle passes it, gets [saddle]
// events as its next turn, and is compacted when its context passes
// compact_at.
func TestJourneyCodexOrchestrator(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{})
	home := codexOnly(t, w)
	codexLog := func() string {
		b, _ := os.ReadFile(filepath.Join(home, "fakecodex.log"))
		return string(b)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("fakecodex.log:\n%s", codexLog())
		}
	})

	out := w.MustSaddle("init", "-q", "--trust").Stdout
	if !strings.Contains(out, `wrote [orchestrator] adapter = "codex"`) {
		t.Fatalf("saddle init didn't put the orchestrator on codex:\n%s", out)
	}
	cfg, err := os.ReadFile(filepath.Join(w.Repo, ".saddle", "config.toml"))
	must(t, err)
	if !strings.Contains(string(cfg), `adapter = "codex"`) {
		t.Fatalf("config.toml lacks [orchestrator] adapter:\n%s", cfg)
	}

	doc := w.Saddle("doctor").Stdout
	for _, want := range []string{"orchestrator", "runs on codex", "not used: the orchestrator runs codex"} {
		if !strings.Contains(doc, want) {
			t.Errorf("doctor lacks %q:\n%s", want, doc)
		}
	}
	for _, line := range strings.Split(doc, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] != "ok" && (f[1] == "orchestrator" || f[1] == "claude") {
			t.Errorf("doctor row not ok: %s", line)
		}
		if strings.HasPrefix(line, "ok") && strings.Contains(line, "orchestrator allowlist") {
			t.Errorf("doctor checked the Claude allowlist for a codex orchestrator: %s", line)
		}
	}

	u := w.StartTUI(140, 44)

	// A tool call over the MCP server from the -c overrides.
	u.Type(`CALL status {}`)
	u.Keys("Enter")
	u.WaitScreen("called status")
	if strings.Contains(u.Screen(), "called status: error") {
		t.Fatalf("the status call failed:\n%s", u.Screen())
	}
	if l := codexLog(); !strings.Contains(l, "CALL status") || !strings.Contains(l, "Your session") {
		t.Errorf("first turn lacks the call or the brief:\n%s", l)
	}

	// A [saddle] event reaches it as a turn of its own.
	must(t, w.App().Notify(app.OrchestratorID, store.NoticeAction, "e2e event: the build broke"))
	Eventually(t, "the event to reach codex", func() error {
		if !strings.Contains(codexLog(), "[saddle] e2e event: the build broke") {
			return errorf("not in fakecodex.log yet")
		}
		return nil
	})
	u.WaitScreen("heard: [saddle]")

	// Past compact_at, saddle compacts it: a summary turn, then a fresh session.
	u.Type("TOKENS 180000")
	u.Keys("Enter")
	u.WaitScreen("heard: TOKENS 180000")
	if err := Poll(3*Timeout(), func() error {
		l := codexLog()
		if !strings.Contains(l, "This session is being compacted") || !strings.Contains(l, "continues one that was compacted") {
			return errorf("not compacted yet")
		}
		return nil
	}); err != nil {
		t.Fatalf("the codex orchestrator was not compacted past compact_at: %v", err)
	}
}
