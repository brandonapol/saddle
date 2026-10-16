package agent

import (
	"slices"

	"github.com/brandonapol/saddle/internal/config"
)

// OrchestratorAdapter is the CLI the orchestrator runs on (#668):
// [orchestrator] adapter when set; the Grok CLI under harness = "grok";
// Claude Code when it is installed; else the harness, then [adapters]
// order, whichever first can run and has a headless session; else Claude
// Code. ss is the machine's availability; nil means unknown, which keeps
// Claude Code.
func OrchestratorAdapter(cfg config.Config, ss []Status) string {
	if a := cfg.Orchestrator.Adapter; a != "" {
		return a
	}
	if cfg.Harness == config.HarnessGrok {
		return config.HarnessGrok
	}
	if ss == nil || installedIn(ss, config.HarnessClaude) {
		return config.HarnessClaude
	}
	if fb, ok := FallbackHarness(cfg.Harness, OrchestratorOrder(cfg), ss); ok && slices.Contains(config.OrchestratorAdapters(), fb) {
		return fb
	}
	return config.HarnessClaude
}

// OrchestratorOrder is the adapters the orchestrator falls back through:
// [adapters] order, then any it leaves out, keeping only those with a
// headless session.
func OrchestratorOrder(cfg config.Config) []string {
	var out []string
	for _, n := range append(slices.Clone(cfg.AdapterOrder), config.OrchestratorAdapters()...) {
		if slices.Contains(config.OrchestratorAdapters(), n) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// OrchestratorModel is the orchestrator's model on adapter: [orchestrator]
// model when adapter is the one [orchestrator] adapter names, else the
// adapter's own key. A failover to another adapter doesn't carry a model
// that adapter can't run. Empty is the CLI's default.
func OrchestratorModel(cfg config.Config, adapter string) string {
	if m := cfg.Orchestrator.Model; m != "" && cfg.Orchestrator.Adapter == adapter {
		return m
	}
	switch adapter {
	case config.HarnessGrok:
		return cfg.Grok.OrchestratorModel
	case config.HarnessCodex:
		return cfg.Adapters[config.HarnessCodex].Model
	}
	return cfg.Claude.OrchestratorModel
}

func installedIn(ss []Status, name string) bool {
	for _, s := range ss {
		if s.Name == name {
			return s.Installed
		}
	}
	return false
}
