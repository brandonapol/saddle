package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

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
	case config.AdapterOpenCode:
		return cfg.Adapters[config.AdapterOpenCode].Model
	}
	return cfg.Claude.OrchestratorModel
}

func installedIn(ss []Status, name string) bool {
	for _, s := range ss {
		if s.Name == name {
			return s.Installed || s.OK
		}
	}
	return false
}

// CodexHome is codex's state dir: $CODEX_HOME, else ~/.codex.
func CodexHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".codex")
}

// CodexSandbox is the sandbox a headless codex orchestrator turn runs in:
// "danger-full-access" when approvals and the sandbox are bypassed, else
// the sandbox_mode the turn sets. mode is [claude] permission_mode and args
// [adapters.codex] args, as the orchestrator's launch gets them.
func CodexSandbox(mode string, args []string) string {
	exec := Launch{Mode: mode, Args: args}.codexExecArgs()
	sandbox := ""
	for i, a := range exec {
		if a == "--dangerously-bypass-approvals-and-sandbox" {
			return "danger-full-access"
		}
		v := a
		if (a == "-c" || a == "--config") && i+1 < len(exec) {
			v = exec[i+1]
		}
		v = strings.TrimPrefix(strings.TrimPrefix(v, "--config="), "-c=")
		if k, val, ok := strings.Cut(v, "="); ok && strings.TrimSpace(k) == "sandbox_mode" {
			sandbox = strings.Trim(strings.TrimSpace(val), `"'`)
		}
	}
	return sandbox
}

// CodexSaddleServerProblem says why a saddle MCP server in codex's own
// config.toml under home would defeat the -c mcp_servers.saddle overrides
// saddle passes each turn, or "". Codex merges the overrides key by key,
// so a url transport or enabled = false the overrides don't set survives.
func CodexSaddleServerProblem(home string) string {
	p := filepath.Join(home, "config.toml")
	var c struct {
		MCPServers map[string]map[string]any `toml:"mcp_servers"`
	}
	if _, err := toml.DecodeFile(p, &c); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ""
		}
		return fmt.Sprintf("%s does not parse: %v", p, err)
	}
	s, ok := c.MCPServers["saddle"]
	if !ok {
		return ""
	}
	if _, ok := s["url"]; ok {
		return fmt.Sprintf("%s defines [mcp_servers.saddle] with a url, which saddle's command override can't replace", p)
	}
	if on, ok := s["enabled"].(bool); ok && !on {
		return fmt.Sprintf("%s sets [mcp_servers.saddle] enabled = false, so the orchestrator gets no saddle tools", p)
	}
	return ""
}
