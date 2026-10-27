package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
)

// #668: with claude missing and codex usable, saddle up's orchestrator runs
// on codex through the codex bridge, with saddle's MCP tools and the brief.
func TestOrchestratorRunsOnCodexWithoutClaude(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.AdapterStatus = func() []agent.Status {
		return []agent.Status{{Name: "claude", Reason: "claude not on PATH"}, {Name: "codex", OK: true, Installed: true}}
	}
	a.Cfg.Adapters = config.AdapterMap{"codex": {Model: "gpt-5-codex"}}
	l, _, err := a.Orchestrator()
	must(t, err)
	if l.Kind != agent.KindCodex || l.Model != "gpt-5-codex" || l.Brief == "" {
		t.Fatalf("launch kind %q model %q brief %d bytes, want codex gpt-5-codex and a brief", l.Kind, l.Model, len(l.Brief))
	}
	id := a.OrchestratorIdentity()
	if id.Adapter != "codex" || id.Hooks || id.Compact != "/compact" {
		t.Errorf("identity = %+v, want codex without hooks, compacting with /compact", id)
	}
	cmd, err := l.Headless("")
	must(t, err)
	if len(cmd.Args) < 2 || cmd.Args[1] != "codex-bridge" {
		t.Errorf("headless process = %q, want saddle codex-bridge", cmd.Args)
	}
	if got := agent.Recorded(l.RunDir); got != "codex" {
		t.Errorf("run dir recorded %q, want codex", got)
	}
}

// #668: [orchestrator] adapter and model pick the orchestrator even when
// Claude Code is installed; without them nothing changes.
func TestOrchestratorAdapterConfig(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	if l, _, err := a.Orchestrator(); err != nil || l.Kind != agent.KindClaude || l.Model != "sonnet" {
		t.Fatalf("default: kind %q model %q, %v; want claude sonnet", l.Kind, l.Model, err)
	}
	a.Cfg.Harness = config.HarnessCodex
	if l, _, err := a.Orchestrator(); err != nil || l.Kind != agent.KindClaude {
		t.Fatalf("harness codex with claude installed: kind %q, %v; want claude", l.Kind, err)
	}
	a.Cfg.Orchestrator.Adapter, a.Cfg.Orchestrator.Model = "codex", "gpt-o"
	if l, _, err := a.Orchestrator(); err != nil || l.Kind != agent.KindCodex || l.Model != "gpt-o" {
		t.Fatalf("configured codex: kind %q model %q, %v", l.Kind, l.Model, err)
	}
	a.Cfg.Orchestrator.Adapter, a.Cfg.Orchestrator.Model = "grok", ""
	a.Cfg.Grok.OrchestratorModel = "grok-o"
	if l, _, err := a.Orchestrator(); err != nil || l.Kind != agent.KindGrok || l.Model != "grok-o" {
		t.Fatalf("configured grok: kind %q model %q, %v", l.Kind, l.Model, err)
	}
}

// #745: [orchestrator] adapter = "opencode" runs the orchestrator through
// the opencode bridge, with [adapters.opencode] model and cmd, no hooks,
// /compact, and the bridge's ledger as its context meter.
func TestOrchestratorRunsOnOpenCode(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.Cfg.Orchestrator.Adapter = "opencode"
	a.Cfg.Adapters = config.AdapterMap{"opencode": {Cmd: "myopencode", Model: "opencode/big-pickle"}}
	l, _, err := a.Orchestrator()
	must(t, err)
	if l.Kind != agent.KindOpenCode || l.Model != "opencode/big-pickle" || l.Cmd != "myopencode" || l.Brief == "" {
		t.Fatalf("launch kind %q model %q cmd %q brief %d bytes", l.Kind, l.Model, l.Cmd, len(l.Brief))
	}
	id := a.OrchestratorIdentity()
	if id.Adapter != "opencode" || id.Hooks || id.Compact != "/compact" || id.CLI() != "opencode" {
		t.Errorf("identity = %+v, want opencode without hooks, compacting with /compact", id)
	}
	if !strings.Contains(l.Brief, "You run on opencode") {
		t.Errorf("brief doesn't name opencode:\n%s", l.Brief)
	}
	cmd, err := l.Headless("")
	must(t, err)
	if len(cmd.Args) < 2 || cmd.Args[1] != "opencode-bridge" || agent.Recorded(l.RunDir) != "opencode" {
		t.Fatalf("headless process = %q, recorded %q", cmd.Args, agent.Recorded(l.RunDir))
	}

	// The context meter reads the bridge's ledger.
	ledger := filepath.Join(l.RunDir, agent.OpenCodeContextFile)
	line := `{"type":"assistant","timestamp":"2026-10-26T12:00:00Z","message":{"id":"prt_1","model":"opencode/big-pickle","usage":{"input_tokens":1000,"cache_read_input_tokens":149000,"output_tokens":5}}}`
	must(t, os.WriteFile(ledger, []byte(line+"\n"), 0o644))
	u, err := a.OrchestratorContext()
	must(t, err)
	if u.Adapter != "opencode" || u.Prompt != 150000 || u.Window != DefaultContextWindow {
		t.Errorf("context = %+v, want 150000 of %d on opencode", u, DefaultContextWindow)
	}
}
