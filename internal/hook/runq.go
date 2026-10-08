package hook

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/runq"
)

// EventHeavyRewrite is logged when the hook routes a Bash command through
// the heavy-run queue.
const EventHeavyRewrite = "runq_rewrite"

// executable is the saddle binary the rewritten command calls; tests
// replace it.
var executable = os.Executable

// readOnly are helpers a heavy line may pipe through or start with and
// still be wrapped (and so pre-approved) whole: they only read, print or
// change directory.
var readOnly = map[string]bool{
	"cd": true, "pwd": true, "echo": true, "printf": true, "true": true, "false": true,
	"tail": true, "head": true, "grep": true, "egrep": true, "fgrep": true, "wc": true,
	"cat": true, "tr": true, "cut": true, "date": true, "sleep": true,
}

// heavyBash routes a heavy Bash command through `saddle run` (#240,
// docs/runq.md Q1(a)), or returns nil to leave it alone. Claude Code only
// applies an updatedInput that comes with an allow or ask decision, and ask
// would stop an autonomous agent at a prompt, so the rewrite allows. That is
// why HeavyRewrite only takes lines made of the repo's tools and read-only
// helpers: anything else keeps its own permission check, and the shims
// still queue the heavy tools inside it. Deny and ask rules still apply to
// the rewritten command.
func heavyBash(a *app.App, task string, in Input) *Output {
	cmd, _ := in.ToolInput["command"].(string)
	if in.Grok || in.ToolName != "Bash" || cmd == "" || in.PermissionMode == "plan" ||
		os.Getenv(runq.EnvBypass) == string(runq.ModeOff) || os.Getenv(runq.EnvLease) != "" {
		return nil
	}
	cfg, err := a.Heavy().Config()
	if err != nil || cfg.Mode == runq.ModeOff {
		return nil // a broken runq.toml must not break every Bash call
	}
	bin, err := executable()
	if err != nil {
		return nil
	}
	out, class := HeavyRewrite(cmd, runq.NewMatcher(cfg), bin)
	if out == "" {
		return nil
	}
	a.Store.Event(task, EventHeavyRewrite, class+": "+cmd)
	updated := maps.Clone(in.ToolInput)
	updated["command"] = out
	return &Output{Specific: &specific{HookEventName: "PreToolUse", PermissionDecision: "allow",
		PermissionDecisionReason: fmt.Sprintf("[saddle] heavy command routed through the machine's heavy-run queue (class %s): "+
			"it starts when a slot is free and runs unchanged. SADDLE_RUNQ=off before the command skips this.", class),
		UpdatedInput: updated}}
}

// HeavyRewrite returns cmd rewritten to `<bin> run --class C --prio worker
// -- cmd`, and C, when cmd runs a heavy tool; "" otherwise. A single simple
// command is prefixed as is; anything else (pipes, &&, VAR=value prefixes)
// is wrapped whole in bash -c, so the line takes one lease.
//
// It leaves cmd alone when it is light, already runs `saddle run`, sets
// SADDLE_RUNQ=off, or has anything besides heavy tools, the repo's other
// tools (the commands patterns start with: go, make, ...) and read-only
// helpers: substitutions, file redirections, background jobs, subshells.
func HeavyRewrite(cmd string, m runq.Matcher, bin string) (string, string) {
	cmds, ok := parseShell(cmd)
	if !ok || len(cmds) == 0 {
		return "", ""
	}
	tools := map[string]bool{}
	for _, b := range m.Binaries() {
		tools[b] = true
	}
	class, wrap := "", len(cmds) > 1
	for _, c := range cmds {
		switch c.sep {
		case "", ";", "\n", "&&", "||", "|":
		default:
			return "", "" // & ( ) |&
		}
		for _, r := range c.redirs {
			if !harmless(r) {
				return "", ""
			}
		}
		w := c.words
		n := assignments(w)
		for _, kv := range w[:n] {
			if kv == runq.EnvBypass+"="+string(runq.ModeOff) {
				return "", ""
			}
		}
		if n > 0 {
			wrap = true
		}
		w = w[n:]
		if len(w) > 0 && w[0] == "time" {
			w, wrap = w[1:], true
		}
		if len(w) == 0 {
			return "", ""
		}
		name := filepath.Base(w[0])
		if name == "saddle" && len(w) > 1 && w[1] == "run" {
			return "", "" // already queued
		}
		if cl := m.Class(w); cl != "" {
			if class == "" {
				class = cl
			}
			continue
		}
		if !readOnly[name] && !tools[name] {
			return "", ""
		}
	}
	if class == "" {
		return "", ""
	}
	prefix := quoteIfNeeded(bin) + " run --class " + class + " --prio worker -- "
	if wrap {
		return prefix + "bash -c " + shellQuote(strings.TrimSpace(cmd)), class
	}
	return prefix + strings.TrimSpace(cmd), class
}

// harmless redirections only duplicate or close descriptors or go to
// /dev/null.
func harmless(r redir) bool {
	op := strings.TrimLeft(r.op, "0123456789")
	switch {
	case op == ">&" || op == "<&":
		return r.target == "" || isDigits(r.target) || r.target == "-"
	case op == ">" || op == ">>" || op == "&>" || op == "&>>" || op == ">|":
		return r.target == "/dev/null"
	}
	return false
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func quoteIfNeeded(s string) string {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("/._-+@%:,=", c)) {
			return shellQuote(s)
		}
	}
	return s
}
