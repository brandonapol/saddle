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

// installFakeOpenCode builds fakeopencode as opencode on w's PATH. It
// returns opencode's data dir under the world's home.
func installFakeOpenCode(t *testing.T, w *World) string {
	t.Helper()
	root, err := moduleRoot()
	must(t, err)
	cmd := exec.Command("go", "build", "-o", filepath.Join(w.Bin, "opencode"), "./internal/agent/agenttest/fakeopencode/cmd/opencode")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), goEnv(t)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fakeopencode: %v\n%s", err, out)
	}
	data := filepath.Join(w.Home, ".local", "share", "opencode")
	must(t, os.MkdirAll(data, 0o755))
	return data
}

// TestJourneyOpenCodeOrchestrator (#745): with [orchestrator] adapter =
// "opencode", doctor names a missing opencode login, and once logged in is
// clean for the orchestrator. saddle up then runs the orchestrator through
// the opencode bridge against a fake opencode: it calls saddle's tools over
// the MCP server in OPENCODE_CONFIG, its later turns resume the session,
// [saddle] events reach it as turns, it is compacted past compact_at, and a
// quota error moves it to claude with a state summary.
func TestJourneyOpenCodeOrchestrator(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{Tables: "[orchestrator]\nadapter = \"opencode\"\nmodel = \"opencode/big-pickle\"\n"})
	data := installFakeOpenCode(t, w)
	ocLog := func() string {
		b, _ := os.ReadFile(filepath.Join(data, "fakeopencode.log"))
		return string(b)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("fakeopencode.log:\n%s", ocLog())
			led, _ := os.ReadFile(filepath.Join(w.Repo, ".saddle", "run", app.OrchestratorID, "opencode-context.jsonl"))
			t.Logf("context ledger:\n%s", led)
			if es, err := w.App().Store.Events(60); err == nil {
				for _, e := range es {
					t.Logf("event %s %s %s", e.Task, e.Kind, e.Data)
				}
			}
		}
	})

	orchRow := func(doc string) string {
		for _, line := range strings.Split(doc, "\n") {
			if f := strings.Fields(line); len(f) >= 2 && f[1] == "orchestrator" {
				return line
			}
		}
		t.Fatalf("doctor has no orchestrator row:\n%s", doc)
		return ""
	}
	// No login: doctor says so and how to fix it.
	doc := w.Saddle("doctor").Stdout
	if row := orchRow(doc); !strings.HasPrefix(row, "warn") || !strings.Contains(row, "opencode is not logged in") {
		t.Errorf("logged-out orchestrator row = %q", row)
	}
	if !strings.Contains(doc, "opencode providers login") {
		t.Errorf("doctor doesn't say how to log in:\n%s", doc)
	}

	must(t, os.WriteFile(filepath.Join(data, "auth.json"), []byte("{}"), 0o600))
	doc = w.Saddle("doctor").Stdout
	if row := orchRow(doc); !strings.HasPrefix(row, "ok") || !strings.Contains(row, "runs on opencode (model opencode/big-pickle)") ||
		!strings.Contains(row, "opencode 1.19.0") {
		t.Errorf("logged-in orchestrator row = %q", row)
	}

	u := w.StartTUI(140, 44)

	// A full turn: a tool call over the MCP server OPENCODE_CONFIG names.
	u.Type(`CALL status {}`)
	u.Keys("Enter")
	u.WaitScreen("called status")
	if strings.Contains(u.Screen(), "called status: error") {
		t.Fatalf("the status call failed:\n%s", u.Screen())
	}
	if l := ocLog(); !strings.Contains(l, "--- turn ses_fake0001") || !strings.Contains(l, "CALL status") || !strings.Contains(l, "Your session") {
		t.Errorf("first turn lacks the session, the call or the brief:\n%s", l)
	}

	// The next turn resumes the same session, without the brief.
	u.Type("second turn")
	u.Keys("Enter")
	u.WaitScreen("heard: second turn")
	if l := ocLog(); strings.Count(l, "--- turn ses_fake0001") != 2 || strings.Count(l, "## Your session") != 1 {
		t.Errorf("second turn didn't resume ses_fake0001:\n%s", l)
	}

	// A [saddle] event reaches it as a turn of its own.
	must(t, w.App().Notify(app.OrchestratorID, store.NoticeAction, "e2e event: the build broke"))
	Eventually(t, "the event to reach opencode", func() error {
		if !strings.Contains(ocLog(), "[saddle] e2e event: the build broke") {
			return errorf("not in fakeopencode.log yet")
		}
		return nil
	})
	u.WaitScreen("heard: [saddle]")

	// Past compact_at, saddle compacts it: a summary turn, then a fresh session.
	u.Type("TOKENS 180000")
	u.Keys("Enter")
	u.WaitScreen("heard: TOKENS 180000")
	if err := Poll(3*Timeout(), func() error {
		l := ocLog()
		if !strings.Contains(l, "This session is being compacted") || !strings.Contains(l, "--- turn ses_fake0002") ||
			!strings.Contains(l, "continues one that was compacted") {
			return errorf("not compacted yet")
		}
		return nil
	}); err != nil {
		t.Fatalf("the opencode orchestrator was not compacted past compact_at: %v", err)
	}

	// A quota error moves the orchestrator to claude, once it can run, with
	// a state summary.
	creds := filepath.Join(w.Home, ".claude", ".credentials.json")
	must(t, os.MkdirAll(filepath.Dir(creds), 0o700))
	must(t, os.WriteFile(creds, []byte("{}"), 0o600))
	u.Type("QUOTA")
	u.Keys("Enter")
	u.WaitScreen("ran out of quota on opencode", "moved it to claude")
}
