package app

import (
	"fmt"
	"slices"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
)

// probeFlag checks one claude flag; tests replace it so they need no binary.
var probeFlag agent.FlagProbe = agent.ProbeFlag

// advisorOn reports whether the orchestrator runs as the hierarchical
// advisor (#257): [claude.advisor] enabled under the Claude harness.
func (a *App) advisorOn() bool {
	return a.Cfg.Claude.Advisor.Enabled && a.Cfg.Harness != config.HarnessGrok
}

// applyAdvisor turns the orchestrator launch into the hierarchical advisor
// layout: the lead model at high effort, Claude Code subagents on the
// subagent model, and the advisor model on call through --advisor. The
// Agent tool is allowed so the subagents can run. A flag the installed claude
// rejects fails the launch; it never falls back to a single model.
func (a *App) applyAdvisor(l *agent.Launch) error {
	if !a.advisorOn() {
		return nil
	}
	ad := a.Cfg.Claude.Advisor
	l.Model, l.Effort, l.Advisor, l.SubagentModel = ad.Lead, "high", ad.Advisor, ad.Subagents
	l.Deny = slices.DeleteFunc(slices.Clone(l.Deny), func(s string) bool { return s == "Agent" })
	for _, f := range [][2]string{{"--effort", l.Effort}, {"--advisor", l.Advisor}} {
		if err := probeFlag(l.Cmd, f[0], f[1]); err != nil {
			return fmt.Errorf("[claude.advisor] is enabled but claude does not accept %s %s (set enabled = false to run without it): %w", f[0], f[1], err)
		}
	}
	return nil
}
