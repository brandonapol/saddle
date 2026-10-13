package app

import (
	"fmt"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/usage"
)

// OrchIdentity is what an orchestrator session runs on (#479), so its brief,
// status and the TUI header can name it and leave out hints its harness
// doesn't have.
type OrchIdentity struct {
	Adapter string // usage.Claude, usage.Codex or usage.Grok
	Model   string // the configured model; "" is the adapter's default
	Workers string // the adapter workers spawned without adapter= use
	Hooks   bool   // saddle's hooks run in the session
	Compact string // the command that compacts it; "" when it has none
}

// OrchestratorIdentity is the TUI's headless orchestrator: grok-bridge under
// harness = "grok", Claude Code otherwise. Codex has no headless session, so
// saddle up codex runs the orchestrator on Claude Code and only workers on codex.
func (a *App) OrchestratorIdentity() OrchIdentity {
	return a.identity(a.orchestratorAgent(), a.orchModel())
}

// PluginIdentity is the user's own session running the saddle plugin, which
// is the harness's CLI.
func (a *App) PluginIdentity() OrchIdentity {
	kind := usage.Claude
	switch a.Cfg.Harness {
	case config.HarnessGrok:
		kind = usage.Grok
	case config.HarnessCodex:
		kind = usage.Codex
	}
	return a.identity(kind, "")
}

func (a *App) identity(kind, model string) OrchIdentity {
	workers := a.Cfg.Harness
	if workers == "" {
		workers = config.HarnessClaude
	}
	cmd, _ := CompactCommand(kind)
	return OrchIdentity{Adapter: kind, Model: model, Workers: workers, Hooks: kind != usage.Codex, Compact: cmd}
}

// CLI names the adapter's program for people.
func (id OrchIdentity) CLI() string { return adapterCLI(id.Adapter) }

// ModelName is the model, or "<adapter>'s default" when none is configured.
func (id OrchIdentity) ModelName() string {
	if id.Model != "" {
		return id.Model
	}
	return id.Adapter + "'s default"
}

func adapterCLI(kind string) string {
	switch kind {
	case usage.Grok:
		return "the Grok CLI"
	case usage.Codex:
		return "Codex"
	}
	return "Claude Code"
}

// Brief is the brief's section on the session itself: what it runs on, where
// workers run, and whether hooks guard it.
func (id OrchIdentity) Brief() string {
	s := fmt.Sprintf("## Your session\n- You run on %s (adapter %s, model %s). Status shows it under orchestrator_context.\n",
		id.CLI(), id.Adapter, id.ModelName())
	if id.Workers != id.Adapter {
		s += fmt.Sprintf("- Workers spawned without adapter= run on %s, not on your CLI.\n", adapterCLI(id.Workers))
	}
	if id.Hooks {
		s += "- Saddle's hooks run in your session: they deny writes to paths another task claimed and carry [saddle] events to you.\n"
	} else {
		s += "- Your session has no saddle hooks: nothing denies writes to paths a task claimed, so don't edit files yourself.\n"
	}
	return s + "\n"
}

// compactBrief tells the session how to compact with its harness's command.
func (id OrchIdentity) compactBrief() string {
	const keep = "running tasks, open PRs, queued follow-ups, owner decisions pending, rules in force"
	const when = "- Compact your context at natural breakpoints: after a batch lands and its PRs merge, before a long planning step, or when saddle tells you context is above the threshold. "
	switch {
	case id.Compact == "":
		return when + fmt.Sprintf("%s has no compact command: write a short state summary (%s) and ask the owner to start a fresh session.\n", id.CLI(), keep)
	case id.Compact == "/compact": // codex and grok take no instruction
		return when + fmt.Sprintf("Write a short state summary first (%s), then run /compact; it takes no instruction. When you are idle and the owner isn't typing, saddle may send the command for you.\n", keep)
	}
	return when + fmt.Sprintf("Run /compact and keep a short state summary: %s. When you are idle and the owner isn't typing, saddle may send the command for you.\n", keep)
}

// pushDenial says where a refused git push comes from on this harness.
func (id OrchIdentity) pushDenial() string {
	if id.Adapter == usage.Claude {
		return "a denied push comes from Claude's auto-mode classifier"
	}
	return "a denied push comes from saddle's guard"
}
