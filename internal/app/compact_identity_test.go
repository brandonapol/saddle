package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/usage"
)

// #479: after saddle up <adapter>, status names the orchestrator's adapter
// and model, and both briefs match the session they brief: no /compact for
// grok, no hooks for a codex session, and workers' adapter named apart.
func TestOrchestratorKnowsItsAdapter(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		harness string
		model   string
		// the TUI's headless orchestrator
		adapter, cli string
		compact      string // how the brief says to compact
		// the user's own session under the plugin
		plugin, pluginCLI string
		pluginHooks       bool
	}{
		{config.HarnessClaude, "claude-opus-5-5", usage.Claude, "Claude Code", "Run /compact", usage.Claude, "Claude Code", true},
		{config.HarnessCodex, "claude-sonnet-5-5", usage.Claude, "Claude Code", "Run /compact", usage.Codex, "Codex", false},
		{config.HarnessGrok, "grok-4-fast", usage.Grok, "the Grok CLI", "then run /compact; it takes no instruction", usage.Grok, "the Grok CLI", true},
	} {
		t.Run(c.harness, func(t *testing.T) {
			t.Parallel()
			a, _ := setup(t)
			a.Cfg.Harness = c.harness
			a.Cfg.Claude.OrchestratorModel = c.model
			a.Cfg.Grok.OrchestratorModel = c.model

			id := a.OrchestratorIdentity()
			if id.Adapter != c.adapter || id.Model != c.model || id.Workers != c.harness {
				t.Fatalf("identity = %+v", id)
			}
			// Status (orchestrator_context) names the adapter and model.
			_, err := a.EnsureOrchestrator()
			must(t, err)
			u, err := a.OrchestratorContext()
			must(t, err)
			if u.Adapter != c.adapter || u.Model != c.model {
				t.Errorf("OrchestratorContext = %s/%s, want %s/%s", u.Adapter, u.Model, c.adapter, c.model)
			}

			brief := a.orchestratorBrief()
			want := "You run on " + c.cli + " (adapter " + c.adapter + ", model " + c.model + ")"
			if !strings.Contains(brief, want) {
				t.Errorf("tui brief lacks %q", want)
			}
			if c.harness != c.adapter && !strings.Contains(brief, "Workers spawned without adapter= run on Codex") {
				t.Error("tui brief does not say workers run on codex")
			}
			if !strings.Contains(brief, c.compact) {
				t.Errorf("tui brief lacks %q", c.compact)
			}
			if c.adapter != usage.Claude && strings.Contains(brief, "Claude's auto-mode classifier") {
				t.Error("non-Claude brief blames Claude's classifier for denied pushes")
			}

			pid := a.PluginIdentity()
			if pid.Adapter != c.plugin || pid.Hooks != c.pluginHooks {
				t.Fatalf("plugin identity = %+v", pid)
			}
			pb := a.PluginBrief()
			if want := "You run on " + c.pluginCLI + " (adapter " + c.plugin; !strings.Contains(pb, want) {
				t.Errorf("plugin brief lacks %q", want)
			}
			if hooks := strings.Contains(pb, "Saddle's hooks run in your session"); hooks != c.pluginHooks {
				t.Errorf("plugin brief claims hooks = %t, want %t", hooks, c.pluginHooks)
			}
		})
	}
}

// With no model configured the brief names the adapter's default, never "".
func TestOrchestratorIdentityDefaultModel(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.Cfg.Claude.OrchestratorModel = ""
	if got := a.OrchestratorIdentity().ModelName(); got != "claude's default" {
		t.Fatalf("model name = %q", got)
	}
	if !strings.Contains(a.orchestratorBrief(), "model claude's default)") {
		t.Error("brief does not name the default model")
	}
}

// A harness with no compact command is told to summarize instead.
func TestCompactBriefWithoutCommand(t *testing.T) {
	t.Parallel()
	b := OrchIdentity{Adapter: "gemini"}.compactBrief()
	if strings.Contains(b, "/compact") || !strings.Contains(b, "has no compact command") {
		t.Fatalf("brief = %q, want a summary and fresh session, not /compact", b)
	}
}
