package mcpserver

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
)

// #479: orchestrator_context names the adapter the orchestrator runs on. Under
// saddle up codex only the workers run codex; the orchestrator is Claude Code.
func TestStatusNamesOrchestratorAdapter(t *testing.T) {
	for _, harness := range []string{config.HarnessClaude, config.HarnessCodex} {
		t.Run(harness, func(t *testing.T) {
			a, _ := stackSetup(t)
			a.Cfg.Harness = harness
			cfgDir := t.TempDir()
			t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
			if _, err := a.EnsureOrchestrator(); err != nil {
				t.Fatal(err)
			}
			if err := a.Store.SetField(app.OrchestratorID, "session_id", "s1"); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(cfgDir, "projects", "-x"), 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, cfgDir, "projects/-x/s1.jsonl", `{"type":"assistant","timestamp":"2026-10-03T10:00:00Z","message":{"id":"m1","model":"claude-opus-4-5","usage":{"input_tokens":1,"output_tokens":5,"cache_read_input_tokens":89999}}}`+"\n")
			c := callStatus(t, a).OrchestratorContext
			if c == nil || c.Adapter != "claude" || c.Model != "claude-opus-4-5" {
				t.Fatalf("orchestrator_context = %+v, want adapter claude, model claude-opus-4-5", c)
			}
		})
	}
}
