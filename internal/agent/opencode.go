package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// OpenCode is the opencode adapter (#509): its interactive TUI in the
// worktree, with the task prompt submitted by --prompt and the saddle MCP
// server in a run-dir config that OPENCODE_CONFIG merges over the user's own,
// so nothing lands in the repo. The same config loads saddle's claims plugin
// (#532), which denies writes to paths another task claims, as the claude and
// grok hooks do. Notices, session ids and idle status still come from no
// hook, so Hooks() is false: claims are also checked at commit and done
// (#489), notices are typed in, and the screen watcher marks it idle.
type OpenCode struct{}

// OpenCodeName is the opencode adapter's name; usage has no opencode parser yet.
const OpenCodeName = "opencode"

const openCodeConfig = "opencode.json"

func (OpenCode) Name() string                 { return OpenCodeName }
func (OpenCode) Hooks() bool                  { return false }
func (OpenCode) Inject(notices string) string { return hooklessInject(notices) }

func (OpenCode) Launch(l Launch) (string, error) {
	if err := os.MkdirAll(l.RunDir, 0o755); err != nil {
		return "", err
	}
	plugin, err := l.writeOpenCodePlugin()
	if err != nil {
		return "", err
	}
	cfg := map[string]any{
		"$schema": "https://opencode.ai/config.json",
		"plugin":  []string{plugin},
		"mcp": map[string]any{"saddle": map[string]any{
			"type": "local", "command": []string{l.Bin, "mcp"}, "environment": l.env(), "enabled": true,
		}},
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(l.RunDir, openCodeConfig), append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	argv := []string{shellQuote(l.cmdOr("opencode"))}
	if l.Model != "" {
		argv = append(argv, "--model", shellQuote(l.Model))
	}
	argv = append(argv, quoteAll(openCodeApproval(l.Mode))...)
	argv = append(argv, quoteAll(l.Args)...)
	argv = append(argv, `--prompt "$(cat "$run/prompt.md")"`)
	setup := []string{
		`export OPENCODE_CONFIG="$run/` + openCodeConfig + `"`,
		// A worker must not stop to upgrade itself mid-task.
		"export OPENCODE_DISABLE_AUTOUPDATE=1",
	}
	return l.writeHookless(OpenCodeName, setup, argv)
}

// openCodeApproval maps a Claude permission mode to opencode flags. opencode
// already allows edits and shell by default; --auto also approves what would
// ask. Workers run unattended, so anything but acceptEdits, plan or default
// gets --auto. plan runs opencode's read-only plan agent.
func openCodeApproval(mode string) []string {
	switch mode {
	case "acceptEdits", "default":
		return nil
	case "plan":
		return []string{"--agent", "plan"}
	}
	return []string{"--auto"}
}

// Usage has no opencode parser yet, so opencode work is not metered.
func (OpenCode) Usage() UsageSource {
	return UsageSource{Agent: OpenCodeName, Transcript: func(string, string, string) string { return "" }}
}
