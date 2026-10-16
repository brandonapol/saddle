package agent

import (
	"testing"

	"github.com/brandonapol/saddle/internal/config"
)

func st(name string, ok, installed bool) Status { return Status{Name: name, OK: ok, Installed: installed} }

// #668: the orchestrator's adapter follows [orchestrator] adapter, then the
// harness, then whichever headless agent can run when Claude Code is missing.
func TestOrchestratorAdapter(t *testing.T) {
	all := []Status{st("claude", true, true), st("codex", true, true), st("grok", true, true)}
	codexOnly := []Status{st("claude", false, false), st("codex", true, true), st("gemini", true, true), st("grok", false, false)}
	grokOnly := []Status{st("claude", false, false), st("codex", false, false), st("grok", true, true)}
	claudeLoggedOut := []Status{st("claude", false, true), st("codex", true, true)}
	none := []Status{st("claude", false, false), st("codex", false, false), st("grok", false, false)}
	cfg := func(harness, adapter string) config.Config {
		c := config.Default()
		c.Harness, c.Orchestrator.Adapter = harness, adapter
		return c
	}
	geminiFirst := cfg("claude", "")
	geminiFirst.AdapterOrder = []string{"gemini", "grok", "codex"}
	for _, tc := range []struct {
		name string
		cfg  config.Config
		ss   []Status
		want string
	}{
		{"default with everything", cfg("claude", ""), all, "claude"},
		{"unknown availability keeps claude", cfg("codex", ""), nil, "claude"},
		{"harness codex keeps an installed claude", cfg("codex", ""), all, "claude"},
		{"harness grok runs grok", cfg("grok", ""), all, "grok"},
		{"configured codex", cfg("claude", "codex"), all, "codex"},
		{"configured grok on a codex harness", cfg("codex", "grok"), all, "grok"},
		{"configured adapter is honoured even when missing", cfg("claude", "codex"), none, "codex"},
		{"codex only", cfg("claude", ""), codexOnly, "codex"},
		{"codex only, harness codex", cfg("codex", ""), codexOnly, "codex"},
		{"grok only", cfg("claude", ""), grokOnly, "grok"},
		{"gemini is skipped: it has no headless session", geminiFirst, codexOnly, "codex"},
		{"claude logged out but installed stays", cfg("claude", ""), claudeLoggedOut, "claude"},
		{"nothing can run names claude", cfg("claude", ""), none, "claude"},
	} {
		if got := OrchestratorAdapter(tc.cfg, tc.ss); got != tc.want {
			t.Errorf("%s: OrchestratorAdapter = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestOrchestratorModel(t *testing.T) {
	c := config.Default()
	c.Grok.OrchestratorModel = "grok-o"
	c.Adapters = config.AdapterMap{"codex": {Model: "gpt-w"}}
	for adapter, want := range map[string]string{"claude": "sonnet", "grok": "grok-o", "codex": "gpt-w"} {
		if got := OrchestratorModel(c, adapter); got != want {
			t.Errorf("unset: OrchestratorModel(%s) = %q, want %q", adapter, got, want)
		}
	}
	c.Orchestrator.Adapter, c.Orchestrator.Model = "codex", "gpt-o"
	if got := OrchestratorModel(c, "codex"); got != "gpt-o" {
		t.Errorf("configured: OrchestratorModel(codex) = %q, want gpt-o", got)
	}
	// A failover to claude must not carry the codex model.
	if got := OrchestratorModel(c, "claude"); got != "sonnet" {
		t.Errorf("failover: OrchestratorModel(claude) = %q, want sonnet", got)
	}
}
