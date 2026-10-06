package app

import (
	"errors"
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/lintgate"
	"github.com/brandonapol/saddle/internal/store"
)

// LintGate is the repo's own pre-commit/lint gate as saddle runs it (#212):
// [train] lint.cmd when set ("" turns it off), else what lintgate detects.
// Cmd is "" when there is no gate to run.
func (a *App) LintGate() lintgate.Gate {
	l := a.Cfg.Train.Lint
	if l.Disabled() {
		return lintgate.Gate{}
	}
	g := lintgate.Detect(a.Root, a.Cfg.Integration, gitx.Run)
	if l.Set {
		g.Kind, g.Source, g.Cmd = "config", ".saddle/config.toml", strings.TrimSpace(l.Cmd)
	}
	return g
}

// lintFailure is what an agent hears when the gate is red: the command, its
// output tail, the fixer if the repo has one, and that --no-verify is no way
// around it.
func lintFailure(g lintgate.Gate, out, intro string) string {
	fix := ""
	if g.Fix != "" {
		fix = fmt.Sprintf(" `%s` may fix some of it.", g.Fix)
	}
	return fmt.Sprintf("%s `%s` failed:\n%s\nThe repo's pre-commit gate rejects this tree.%s Fix it, commit (never with --no-verify), and call the saddle done tool again.",
		intro, g.Cmd, tail(out, 40), fix)
}

// lintDone runs the gate in t's worktree before done queues it, so an agent
// can't report done on a tree the repo's gate rejects.
func (a *App) lintDone(t store.Task) error {
	g := a.LintGate()
	if g.Cmd == "" {
		return nil
	}
	out, err := runShell(t.Worktree, g.Cmd)
	if err == nil || a.brokenGate(t.ID, g, out) {
		return nil
	}
	a.Store.Event(t.ID, "lint_failed", g.Cmd)
	return errors.New(lintFailure(g, out, "done refused: in your worktree"))
}

// trainLint runs the gate on the rebased tree in dir after the tests. It
// returns the failure message, "" when green, off, or the same command the
// tests just ran.
func (a *App) trainLint(id, dir string) string {
	g := a.LintGate()
	if g.Cmd == "" || g.Cmd == strings.TrimSpace(a.Cfg.Test.Cmd) {
		return ""
	}
	out, err := runShell(dir, g.Cmd)
	if err == nil || a.brokenGate(id, g, out) {
		return ""
	}
	return lintFailure(g, out, "Your branch rebased cleanly onto "+a.Cfg.Integration+" and passed its tests, but on the result")
}

// noRule is make's complaint about a target the makefile lacks.
const noRule = "No rule to make target"

// brokenGate reports whether a red gate failed because the gate itself is
// wrong: make has no rule for its target, as when `make fi` was detected
// (#228). No agent can fix that by changing its branch, so it is not
// counted against the branch (or its max_attempts). The orchestrator hears
// about it with the way out: set [train] lint.cmd.
func (a *App) brokenGate(id string, g lintgate.Gate, out string) bool {
	if !strings.Contains(out, noRule) {
		return false
	}
	a.Store.Event(id, "lint_gate_broken", g.Cmd)
	_ = a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
		"The repo's lint gate `%s` (from %s) is broken: %s, so done and the train skipped it for %s. "+
			"Set [train] lint.cmd in .saddle/config.toml to the right command (or \"\" to turn it off); the train picks it up on its next land.\n%s",
		g.Cmd, gateSource(g), noRule, id, tail(out, 5)))
	return true
}

func gateSource(g lintgate.Gate) string {
	if g.Source == "" {
		return g.Kind
	}
	return g.Source
}
