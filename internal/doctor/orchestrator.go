package doctor

import (
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
)

// CheckOrchestrator names the CLI saddle up's orchestrator runs on and
// checks that CLI's own pieces (#668).
const CheckOrchestrator = "orchestrator"

const orchestratorAbout = "the agent CLI saddle up's orchestrator runs on, and what it needs to call saddle's tools"

// codexHome is codex's state dir. Tests replace it.
var codexHome = defaultCodexHome

var defaultCodexHome = agent.CodexHome

// orchAdapter is the adapter the orchestrator runs on, as saddle up picks it.
func (r *run) orchAdapter() string {
	return agent.OrchestratorAdapter(r.cfg, r.agentStatus())
}

// orchestrator reports the orchestrator's adapter and model and, for codex
// and opencode, what their headless turns need. For codex: the CLI, a
// login, exec --json, a saddle MCP server saddle's overrides can replace,
// and a sandbox gh and saddle can work in.
func (r *run) orchestrator() Result {
	adapter := r.orchAdapter()
	model := agent.OrchestratorModel(r.cfg, adapter)
	if model == "" {
		model = adapter + "'s default"
	}
	on := fmt.Sprintf("runs on %s (model %s)", adapter, model)
	switch {
	case r.cfg.Orchestrator.Adapter != "":
		on += ", set by [orchestrator] adapter"
	case adapter != config.HarnessClaude && adapter != r.cfg.Harness:
		on += ", since claude can't run"
	}
	res := r.orchestratorOn(adapter, on)
	res.About = orchestratorAbout
	return res
}

func (r *run) orchestratorOn(adapter, on string) Result {
	var st *agent.Status
	for i, s := range r.agentStatus() {
		if s.Name == adapter {
			st = &r.agentStatus()[i]
		}
	}
	if st != nil && !st.Installed {
		fix := agent.InstallHint(adapter)
		if fb, ok := agent.FallbackHarness("", agent.OrchestratorOrder(r.cfg), r.agentStatus()); ok && r.cfg.Orchestrator.Adapter != "" {
			fix += fmt.Sprintf("; or set [orchestrator] adapter = %q in .saddle/config.toml", fb)
		}
		return fail(CheckOrchestrator, on+", but "+st.Reason, fix)
	}
	switch adapter {
	case config.HarnessCodex:
		return r.codexOrchestrator(on, st)
	case config.AdapterOpenCode:
		return r.openCodeOrchestrator(on, st)
	case config.HarnessGrok:
		return ok(CheckOrchestrator, on+"; see "+CheckGrok)
	}
	return ok(CheckOrchestrator, on+"; see "+CheckClaude+", "+CheckOrchAllow+" and "+CheckSkills)
}

func (r *run) codexOrchestrator(on string, st *agent.Status) Result {
	prog := "codex"
	if c := r.cfg.Adapters[config.HarnessCodex].Cmd; c != "" {
		prog = program(c)
	}
	ver, err := r.env.Exec(prog, "--version")
	if err != nil {
		return fail(CheckOrchestrator, on+", but `"+prog+" --version` fails: "+firstLine(err.Error()),
			"run `"+prog+" --version` yourself and fix what it reports, or "+agent.InstallHint(config.HarnessCodex))
	}
	on += "; " + firstLine(ver)
	if help, err := r.env.Exec(prog, "exec", "--help"); err != nil || !strings.Contains(help, "--json") {
		return fail(CheckOrchestrator, on+", but `"+prog+" exec` has no --json, so it can't run headless",
			"update the Codex CLI: npm install -g @openai/codex@latest")
	}
	if p := agent.CodexSaddleServerProblem(codexHome()); p != "" {
		return fail(CheckOrchestrator, on+", but "+p,
			"remove the [mcp_servers.saddle] table from codex's config.toml; saddle passes its own saddle server to every orchestrator turn")
	}
	if st != nil && !st.OK {
		return warn(CheckOrchestrator, on+", but "+st.Reason,
			"run `codex login`, or set OPENAI_API_KEY, if it isn't logged in some other way")
	}
	codex := r.cfg.Adapters[config.HarnessCodex]
	switch sb := agent.CodexSandbox(r.cfg.Claude.PermissionMode, codex.Args); sb {
	case "read-only":
		return warn(CheckOrchestrator, on+", but its turns run in codex's read-only sandbox, where gh, git fetch and saddle's state writes fail",
			`remove sandbox_mode from [adapters.codex] args, or set [claude] permission_mode = "auto", so the orchestrator runs with --dangerously-bypass-approvals-and-sandbox`)
	case "workspace-write":
		if !strings.Contains(strings.Join(codex.Args, " "), "network_access=true") {
			return warn(CheckOrchestrator, on+", but its turns run in codex's workspace-write sandbox without network, where gh and git fetch fail",
				`add "-c", "sandbox_workspace_write.network_access=true" to [adapters.codex] args, or drop the sandbox_mode there`)
		}
		on += "; sandbox workspace-write with network"
	default:
		on += "; no approvals or sandbox (it runs saddle, gh and read-only git)"
	}
	return ok(CheckOrchestrator, on+"; saddle's MCP tools are passed to every turn; "+orchNoHooksNote)
}

// openCodeOrchestrator checks what the opencode bridge's turns need (#745):
// the CLI, `opencode run --format json`, and a login.
func (r *run) openCodeOrchestrator(on string, st *agent.Status) Result {
	prog := "opencode"
	if c := r.cfg.Adapters[config.AdapterOpenCode].Cmd; c != "" {
		prog = program(c)
	}
	ver, err := r.env.Exec(prog, "--version")
	if err != nil {
		return fail(CheckOrchestrator, on+", but `"+prog+" --version` fails: "+firstLine(err.Error()),
			"run `"+prog+" --version` yourself and fix what it reports, or "+agent.InstallHint(config.AdapterOpenCode))
	}
	on += "; opencode " + firstLine(ver)
	if help, err := r.env.Exec(prog, "run", "--help"); err != nil || !strings.Contains(help, "--format") {
		return fail(CheckOrchestrator, on+", but `"+prog+" run` has no --format, so it can't run headless",
			"update opencode: npm install -g opencode-ai@latest")
	}
	if st != nil && !st.OK {
		return warn(CheckOrchestrator, on+", but "+st.Reason,
			"run `opencode providers login`, or set a provider key such as OPENCODE_API_KEY")
	}
	return ok(CheckOrchestrator, on+"; saddle's MCP tools reach every turn through OPENCODE_CONFIG, and its permission config denies edits and history-moving git; "+orchNoHooksNote)
}

// orchNoHooksNote is what a codex or opencode orchestrator lacks next to Claude Code.
const orchNoHooksNote = "no hooks, so [saddle] events reach it between turns, not mid-turn"

// needsClaude reports whether this run starts Claude Code: as the
// orchestrator, or as the harness workers run in.
func (r *run) needsClaude() bool {
	if r.orchAdapter() == config.HarnessClaude {
		return true
	}
	harness := r.cfg.Harness
	if fb, ok := r.fallback(); ok {
		harness = fb
	}
	return harness == config.HarnessClaude
}
