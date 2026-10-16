package doctor

import (
	"fmt"
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
)

// CheckAgents is which agent CLIs are installed and logged in (#646).
const CheckAgents = "agent CLIs"

// AgentsEnv is an Env that knows which agent CLIs can run here. The check
// is skipped for envs without it.
type AgentsEnv interface {
	Agents(cfg config.Config) []agent.Status
}

// AgentCmds is the binary configured for each adapter, for agent.Availability.
func AgentCmds(cfg config.Config) map[string]string {
	cmds := map[string]string{config.HarnessClaude: program(cfg.Claude.Cmd), config.HarnessGrok: program(cfg.Grok.Cmd)}
	for n, c := range cfg.Adapters {
		if c.Cmd != "" {
			cmds[n] = program(c.Cmd)
		}
	}
	return cmds
}

// HarnessOrder is the harnesses in the order saddle falls back through them:
// [adapters] order, then any it leaves out.
func HarnessOrder(cfg config.Config) []string {
	var out []string
	for _, n := range append(slices.Clone(cfg.AdapterOrder), config.Harnesses()...) {
		if slices.Contains(config.Harnesses(), n) && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out
}

// NoHooksNote is what to know before running codex.
const NoHooksNote = "Codex has no hooks, so claims are checked only at commit and done, not on each edit"

// agentStatus is the agent CLIs' availability, read once; nil without AgentsEnv.
func (r *run) agentStatus() []agent.Status {
	if r.agentsRead {
		return r.agents
	}
	r.agentsRead = true
	if ae, ok := r.env.(AgentsEnv); ok {
		r.agents = ae.Agents(r.cfg)
	}
	return r.agents
}

// fallback is the harness saddle up runs when the configured one can't.
func (r *run) fallback() (string, bool) {
	ss := r.agentStatus()
	if ss == nil {
		return "", false
	}
	return agent.FallbackHarness(r.cfg.Harness, HarnessOrder(r.cfg), ss)
}

// claude checks the Claude Code CLI. Missing, it only fails when no other
// agent CLI can stand in for it.
func (r *run) claude() Result {
	res := r.tool(CheckClaude, r.cfg.Claude.Cmd, "--version", "install Claude Code (https://claude.com/claude-code) or set [claude] cmd in .saddle/config.toml")
	if res.Status != Fail {
		return res
	}
	if fb, ok := r.fallback(); ok && fb != config.HarnessClaude {
		res.Status = Warn
		res.Detail += "; saddle up runs " + fb + " instead (see " + CheckAgents + ")"
	}
	return res
}

// agentCLIs reports which agent CLIs can run, and what saddle up runs when
// the configured harness can't.
func (r *run) agentCLIs() Result {
	ss := r.agentStatus()
	harness := r.cfg.Harness
	order := HarnessOrder(r.cfg)
	var usable, why []string
	for _, s := range ss {
		if s.OK {
			name := s.Name
			if !s.Hooks {
				name += " (no hooks)"
			}
			usable = append(usable, name)
		} else if slices.Contains(order, s.Name) {
			why = append(why, s.Reason)
		}
	}
	fb, found := agent.FallbackHarness(harness, order, ss)
	if found && fb == harness {
		detail := fmt.Sprintf("harness %s is ready; usable: %s", harness, strings.Join(usable, ", "))
		if fb == config.HarnessCodex {
			detail += "; " + NoHooksNote
		}
		return ok(CheckAgents, detail)
	}
	reason, installed := harness+" can't run", false
	for _, s := range ss {
		if s.Name == harness {
			reason, installed = s.Reason, s.Installed
		}
	}
	if installed {
		// No credentials file is only a guess at logged out, so up keeps it.
		detail := fmt.Sprintf("harness %s is installed but looks logged out (%s); saddle up starts it anyway", harness, reason)
		fix := "run " + agent.LoginHint(harness) + " if it isn't logged in some other way"
		if found && fb != harness {
			detail += "; usable instead: " + strings.Join(usable, ", ")
			fix += fmt.Sprintf(", or set harness = %q in .saddle/config.toml", fb)
		}
		return warn(CheckAgents, detail, fix)
	}
	if !found {
		hints := make([]string, len(order))
		for i, n := range order {
			hints[i] = agent.InstallHint(n)
		}
		return fail(CheckAgents, "no agent CLI is installed and logged in: "+strings.Join(why, "; "),
			strings.Join(hints, "; or "))
	}
	detail := fmt.Sprintf("harness %s can't run (%s); %s can, so saddle up runs %s; usable: %s", harness, reason, fb, fb, strings.Join(usable, ", "))
	if fb == config.HarnessCodex {
		detail += "; " + NoHooksNote
	}
	return warn(CheckAgents, detail, fmt.Sprintf("make %s the default: set harness = %q in .saddle/config.toml (saddle init writes it when %s can't run); or %s",
		fb, fb, harness, agent.InstallHint(harness)))
}
