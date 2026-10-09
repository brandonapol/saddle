// Package agent launches coding agents through adapters (see Adapter).
// KindGrok launches the Grok CLI harness instead of Claude Code.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/config"
)

// Kind selects which CLI a launch drives. Empty means Claude Code.
const (
	KindClaude = "claude"
	KindGrok   = "grok"
)

// Launch describes one agent session to start in a tmux window.
type Launch struct {
	Root     string // main checkout
	Bin      string // absolute path to the saddle binary
	Kind     string // KindClaude or KindGrok; empty means Claude
	Task     string
	Title    string
	Dir      string // working directory (the task worktree, or root for the orchestrator)
	Model    string
	Mode     string   // claude --permission-mode
	Cmd      string   // claude executable
	Brief    string   // appended to the system prompt
	Prompt   string   // first user message
	RunDir   string   // where launch files are written
	Allow    []string // permission allow rules; nil means the worker defaults
	Deny     []string // tools removed from the session entirely
	Args     []string // extra CLI arguments (Codex and Grok adapters)
	Resume   string   // Claude session to resume in the window; empty starts a new one
	ExtraEnv map[string]string
	// ShimDir holds the heavy-run shims (#240); it goes first on PATH.
	ShimDir string

	// The hierarchical advisor (#257). Empty fields add nothing to the launch.
	Effort        string // claude --effort
	Advisor       string // claude --advisor model
	SubagentModel string // SubagentModelEnv
}

// claudeExtras are the advisor-mode arguments, in launch order.
func (l Launch) claudeExtras() []string {
	var args []string
	if l.Effort != "" {
		args = append(args, "--effort", l.Effort)
	}
	if l.Advisor != "" {
		args = append(args, "--advisor", l.Advisor)
	}
	return args
}

// WorkerAllow pre-approves edits and routine git: saddle's PreToolUse hook is
// the write guard, so a permission prompt would only stall the agent.
func WorkerAllow(bin string) []string {
	return []string{"mcp__saddle", "Edit", "Write", "MultiEdit", "NotebookEdit",
		"Bash(saddle sync:*)", "Bash(saddle status:*)", "Bash(" + bin + " sync:*)",
		"Bash(git add:*)", "Bash(git commit:*)", "Bash(git status:*)", "Bash(git diff:*)",
		"Bash(git log:*)", "Bash(git rebase --continue:*)"}
}

// OrchestratorDeny removes tools the orchestrator must not use: saddle wakes it
// on every change, so polling and scheduling only burn tokens, and it never
// edits. Only the merge train moves branches, so git commands that do are out too.
func OrchestratorDeny() []string {
	return []string{"ScheduleWakeup", "CronCreate", "Monitor", "Edit", "Write", "MultiEdit", "NotebookEdit", "Agent",
		"Bash(git merge:*)", "Bash(git rebase:*)", "Bash(git reset:*)", "Bash(git push:*)",
		"Bash(git branch -f:*)", "Bash(git update-ref:*)", "Bash(git checkout:*)"}
}

// OrchestratorAllow lets the chat agent read the repo and GitHub and run
// skills (#255), never edit.
// The rules pre-approve what the orchestrator is meant to run, so neither a
// permission prompt nor the auto-mode classifier blocks it (#220): saddle
// itself (writeFiles adds the absolute binary too), gh pr and gh issue, git
// fetch and read-only git. git push is not here and OrchestratorDeny removes
// it: when prs is blocked, saddle publish pushes, as the saddle process.
func OrchestratorAllow() []string {
	return []string{"mcp__saddle", "Read", "Glob", "Grep", "Skill", saddleAllow,
		"Bash(gh issue:*)", "Bash(gh pr:*)", "Bash(gh pr create:*)", "Bash(gh pr edit:*)", "Bash(gh pr view:*)",
		"Bash(gh pr list:*)", "Bash(gh pr merge:*)", "Bash(gh pr checks:*)", "Bash(gh pr diff:*)",
		"Bash(git fetch:*)", "Bash(git log:*)", "Bash(git status:*)", "Bash(git diff:*)", "Bash(git show:*)",
		"Bash(git rev-parse:*)", "Bash(git rev-list:*)", "Bash(git merge-base:*)", "Bash(git ls-files:*)",
		"Bash(git ls-remote:*)", "Bash(git cherry:*)", "Bash(git blame:*)", "Bash(git grep:*)",
		"Bash(git branch --list:*)", "Bash(git worktree list:*)", "Bash(git remote -v:*)"}
}

// saddleAllow pre-approves the saddle CLI by name; writeFiles adds the
// binary's absolute path beside it.
const saddleAllow = "Bash(saddle:*)"

func (l Launch) env() map[string]string {
	env := map[string]string{"SADDLE_ROOT": l.Root, "SADDLE_TASK": l.Task}
	if h := os.Getenv(config.HarnessEnv); h != "" {
		env[config.HarnessEnv] = h // saddle up <agent> (#150)
	}
	for k, v := range l.ExtraEnv {
		env[k] = v
	}
	return env
}

// pathDirs are prepended to the agent's PATH: the heavy-run shims first, so
// they shadow real tools even in saddle's own bin dir, then saddle's.
func (l Launch) pathDirs() []string {
	dirs := []string{filepath.Dir(l.Bin)}
	if l.ShimDir != "" {
		dirs = append([]string{l.ShimDir}, dirs...)
	}
	return dirs
}

// shellPath is pathDirs for a launch script's export PATH line.
func (l Launch) shellPath() string { return strings.Join(quoteAll(l.pathDirs()), ":") }

// envPath is a child process's PATH: pathDirs, then this process's PATH.
func (l Launch) envPath() string {
	return strings.Join(append(l.pathDirs(), os.Getenv("PATH")), string(os.PathListSeparator))
}

// writeFiles writes settings (hooks, permissions), the MCP config, the brief and the prompt.
func (l Launch) writeFiles() error {
	if err := os.MkdirAll(l.RunDir, 0o755); err != nil {
		return err
	}
	hook := func() []map[string]any {
		return []map[string]any{{"type": "command", "command": shellQuote(l.Bin) + " hook", "timeout": 10}}
	}
	allow := l.Allow
	if allow == nil {
		allow = WorkerAllow(l.Bin)
	} else if slices.Contains(allow, saddleAllow) && l.Bin != "" {
		allow = append(slices.Clone(allow), "Bash("+l.Bin+":*)")
	}
	settings := map[string]any{
		"hooks": map[string]any{
			"PreToolUse":       []any{map[string]any{"matcher": "Edit|Write|MultiEdit|NotebookEdit|Bash", "hooks": hook()}},
			"PostToolUse":      []any{map[string]any{"matcher": "*", "hooks": hook()}},
			"UserPromptSubmit": []any{map[string]any{"hooks": hook()}},
			"Stop":             []any{map[string]any{"hooks": hook()}},
			"Notification":     []any{map[string]any{"hooks": hook()}},
			"SessionStart":     []any{map[string]any{"hooks": hook()}},
		},
		"permissions": map[string]any{"allow": allow},
	}
	mcp := map[string]any{"mcpServers": map[string]any{
		"saddle": map[string]any{"type": "stdio", "command": l.Bin, "args": []string{"mcp"}, "env": l.env()},
	}}
	files := map[string]string{
		"brief.md":  l.Brief,
		"prompt.md": l.Prompt,
	}
	for name, v := range map[string]any{"settings.json": settings, "mcp.json": mcp} {
		b, err := json.MarshalIndent(v, "", "  ")
		if err != nil {
			return err
		}
		files[name] = string(b)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(l.RunDir, name), []byte(body), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Write creates the launch files and returns the shell command that runs an
// interactive agent session in a tmux window.
func (l Launch) Write() (string, error) {
	if l.Kind == KindGrok {
		return l.writeGrok()
	}
	return l.writeClaude()
}

// Headless builds the process the TUI talks to. Claude speaks stream-json on
// stdin and stdout. Grok has no persistent stdin protocol, so the process is
// saddle's grok-bridge, which runs one headless grok turn per user message.
func (l Launch) Headless(resume string) (*exec.Cmd, error) {
	if l.Kind == KindGrok {
		return l.headlessGrok(resume)
	}
	return l.headlessClaude(resume)
}

// writeClaude creates the launch files and returns the shell command that runs an
// interactive Claude Code session in a tmux window.
func (l Launch) writeClaude() (string, error) {
	if err := l.writeFiles(); err != nil {
		return "", err
	}
	env := l.env()

	var sh strings.Builder
	sh.WriteString("#!/usr/bin/env bash\n# Generated by saddle. Re-run to restart this agent.\n")
	exportSaddleEnv(&sh, env)
	if l.SubagentModel != "" {
		fmt.Fprintf(&sh, "export %s=%s\n", SubagentModelEnv, shellQuote(l.SubagentModel))
	}
	fmt.Fprintf(&sh, "export PATH=%s:\"$PATH\"\n", l.shellPath())
	fmt.Fprintf(&sh, "cd %s || exit 1\n", shellQuote(l.Dir))
	fmt.Fprintf(&sh, "run=%s\n", shellQuote(l.RunDir))
	args := []string{shellQuote(l.Cmd)}
	if l.Model != "" {
		args = append(args, "--model", shellQuote(l.Model))
	}
	for _, a := range l.claudeExtras() {
		args = append(args, shellQuote(a))
	}
	if l.Mode != "" {
		args = append(args, "--permission-mode", shellQuote(l.Mode))
	}
	args = append(args,
		"--settings", `"$run/settings.json"`,
		"--mcp-config", `"$run/mcp.json"`,
		"-n", shellQuote("saddle "+l.Task+": "+l.Title),
		"--append-system-prompt", `"$(cat "$run/brief.md")"`,
	)
	if len(l.Deny) > 0 {
		args = append(args, "--disallowedTools", shellQuote(strings.Join(l.Deny, ",")))
	}
	if l.Resume != "" {
		args = append(args, "--resume", shellQuote(l.Resume))
	}
	if l.Prompt != "" {
		args = append(args, `"$(cat "$run/prompt.md")"`)
	}
	sh.WriteString(strings.Join(args, " ") + "\n")
	fmt.Fprintf(&sh, "%s exited %s >/dev/null 2>&1\n", shellQuote(l.Bin), shellQuote(l.Task))
	sh.WriteString("echo; echo \"[saddle] agent exited. Re-run: bash $run/launch.sh\"; exec \"${SHELL:-bash}\"\n")

	launch := filepath.Join(l.RunDir, "launch.sh")
	if err := os.WriteFile(launch, []byte(sh.String()), 0o755); err != nil {
		return "", err
	}
	return "bash " + shellQuote(launch), nil
}

// SubagentModelEnv is the variable Claude Code reads for its subagents'
// model. claude has no --subagents flag (2.1.287 rejects it).
const SubagentModelEnv = "CLAUDE_CODE_SUBAGENT_MODEL"

// FlagProbe reports an error, naming flag, when the claude binary cmd does
// not accept flag with value.
type FlagProbe func(cmd, flag, value string) error

// ProbeFlag runs cmd with flag, --print and no input, which exits right after
// option parsing without starting a session. claude --help cannot be used:
// it hides some options (--advisor) and short-circuits before validation.
func ProbeFlag(cmd, flag, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, cmd, flag, value, "--print")
	c.Stdin = strings.NewReader("")
	out, err := c.CombinedOutput()
	if err == nil || bytes.Contains(out, []byte("Input must be provided")) {
		return nil
	}
	msg := strings.TrimSpace(string(out))
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	if msg == "" {
		msg = err.Error()
	}
	return fmt.Errorf("%s rejected %s %s: %s", cmd, flag, value, msg)
}

// exportSaddleEnv writes the launch.sh exports of saddle's own variables.
func exportSaddleEnv(sh *strings.Builder, env map[string]string) {
	for _, k := range []string{"SADDLE_ROOT", "SADDLE_TASK", config.HarnessEnv} {
		if v, ok := env[k]; ok {
			fmt.Fprintf(sh, "export %s=%s\n", k, shellQuote(v))
		}
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// headlessClaude builds a Claude Code process that speaks stream-json on stdin and
// stdout, for an agent whose conversation saddle renders itself. resume, if
// set, continues an earlier session.
func (l Launch) headlessClaude(resume string) (*exec.Cmd, error) {
	if err := l.writeFiles(); err != nil {
		return nil, err
	}
	args := []string{"-p", "--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose", "--include-partial-messages",
		"--settings", filepath.Join(l.RunDir, "settings.json"),
		"--mcp-config", filepath.Join(l.RunDir, "mcp.json"),
		"--append-system-prompt", l.Brief,
	}
	if l.Model != "" {
		args = append(args, "--model", l.Model)
	}
	args = append(args, l.claudeExtras()...)
	if l.Mode != "" {
		args = append(args, "--permission-mode", l.Mode)
	}
	if len(l.Deny) > 0 {
		args = append(args, "--disallowedTools", strings.Join(l.Deny, ","))
	}
	if resume != "" {
		args = append(args, "--resume", resume)
	}
	cmd := exec.Command(l.Cmd, args...)
	cmd.Dir = l.Dir
	cmd.Env = os.Environ()
	for k, v := range l.env() {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if l.SubagentModel != "" {
		cmd.Env = append(cmd.Env, SubagentModelEnv+"="+l.SubagentModel)
	}
	cmd.Env = append(cmd.Env, "PATH="+l.envPath())
	return cmd, nil
}
