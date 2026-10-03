// Package fakeagent is a scripted stand-in for Claude Code in saddle's e2e
// tests. Saddle launches it through the Claude adapter ([claude] cmd), with
// the same arguments it gives claude. Like Claude Code it runs the hooks in
// --settings (SessionStart, PreToolUse on writes, PostToolUse, Stop,
// UserPromptSubmit, Notification) and talks to `saddle mcp`, so status,
// claims and notices go through saddle's real paths.
//
// What it does comes from a JSON Script, <dir>/<task>.json, where dir is
// --script-dir. Everything it sees and does is appended to <dir>/<task>.log
// for tests to wait on.
package fakeagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Script is one agent's plan.
type Script struct {
	Steps []Step `json:"steps"`
}

// Step is one action. Exactly one field besides Optional is set.
type Step struct {
	// Write writes Content to the file Write (relative to the worktree)
	// after asking the PreToolUse hook; a denied write is logged and skipped.
	Write   string `json:"write,omitempty"`
	Append  string `json:"append,omitempty"`
	Content string `json:"content,omitempty"`
	Remove  string `json:"remove,omitempty"`
	// Commit stages everything and commits with this message.
	Commit string `json:"commit,omitempty"`
	// Run runs a shell command in the worktree.
	Run string `json:"run,omitempty"`
	// Done calls `saddle done -s <Done>`.
	Done string `json:"done,omitempty"`
	// MCP calls a saddle MCP tool with Args.
	MCP  string         `json:"mcp,omitempty"`
	Args map[string]any `json:"args,omitempty"`
	// Notify sends a Notification hook, the way Claude Code reports a
	// permission prompt ("Claude needs your permission…") or idling.
	Notify string `json:"notify,omitempty"`
	// Wait goes idle (Stop hook, prompt) until an input or a hook's context
	// contains Wait. Inputs seen before the wait count.
	Wait string `json:"wait,omitempty"`
	// Resolve finishes a rebase stopped on conflicts: writes Content to
	// Resolve, stages it and continues.
	Resolve string `json:"resolve,omitempty"`
	// Exit ends the agent with this code (as a crash or /exit would).
	Exit *int `json:"exit,omitempty"`
	// Optional steps may fail without stopping the script.
	Optional bool `json:"optional,omitempty"`
}

// Kind names the step for the log.
func (s Step) Kind() string {
	switch {
	case s.Write != "":
		return "write " + s.Write
	case s.Append != "":
		return "append " + s.Append
	case s.Remove != "":
		return "remove " + s.Remove
	case s.Commit != "":
		return "commit"
	case s.Run != "":
		return "run"
	case s.Done != "":
		return "done"
	case s.MCP != "":
		return "mcp " + s.MCP
	case s.Notify != "":
		return "notify"
	case s.Wait != "":
		return "wait " + s.Wait
	case s.Resolve != "":
		return "resolve " + s.Resolve
	case s.Exit != nil:
		return fmt.Sprintf("exit %d", *s.Exit)
	}
	return "noop"
}

// ScriptPath is where the agent for task reads its script.
func ScriptPath(dir, task string) string { return filepath.Join(dir, task+".json") }

// LogPath is where the agent for task logs.
func LogPath(dir, task string) string { return filepath.Join(dir, task+".log") }

// OrchestratorLog is where the headless (orchestrator) mode logs.
func OrchestratorLog(dir string) string { return filepath.Join(dir, "orchestrator.log") }

// Save writes s for task.
func (s Script) Save(dir, task string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ScriptPath(dir, task), b, 0o644)
}

// Load reads task's script. A missing script is an empty one: the agent
// starts, reports idle and waits.
func Load(dir, task string) (Script, error) {
	var s Script
	b, err := os.ReadFile(ScriptPath(dir, task))
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("%s: %w", ScriptPath(dir, task), err)
	}
	return s, nil
}

// Builders for readable scripts in tests.

func Write(path, content string) Step  { return Step{Write: path, Content: content} }
func Append(path, content string) Step { return Step{Append: path, Content: content} }
func Commit(msg string) Step           { return Step{Commit: msg} }
func Run(cmd string) Step              { return Step{Run: cmd} }
func Done(summary string) Step         { return Step{Done: summary} }
func Wait(match string) Step           { return Step{Wait: match} }
func Notify(msg string) Step           { return Step{Notify: msg} }
func Resolve(path, content string) Step {
	return Step{Resolve: path, Content: content}
}
func MCP(tool string, args map[string]any) Step { return Step{MCP: tool, Args: args} }
func Exit(code int) Step                        { return Step{Exit: &code} }
