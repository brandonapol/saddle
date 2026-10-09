package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

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
// can't report done on a tree the repo's gate rejects. Like the repo-hook
// wrapper (refguard), it waits its turn on the hooks' lock, so it never runs
// beside another worktree's check, and a committed tree that already passed
// (this gate, or the repo's pre-commit hook for a detected gate) isn't
// checked again (#271).
func (a *App) lintDone(t store.Task) error {
	g := a.LintGate()
	if g.Cmd == "" {
		return nil
	}
	state, tree := hooksState(t.Worktree), cleanTree(t.Worktree)
	if passedLint(state, tree, g) {
		a.Store.Event(t.ID, "lint_skipped", "tree "+tree+" already passed")
		return nil
	}
	unlock := lockHooks(state, a.GateTimeout())
	defer unlock()
	if passedLint(state, tree, g) { // the hook we waited on passed it
		a.Store.Event(t.ID, "lint_skipped", "tree "+tree+" already passed")
		return nil
	}
	defer a.sweepScratchAfter(t.ID)
	for attempt := 0; ; attempt++ {
		out, err := a.runLintGate(t.Worktree, g.Cmd)
		if errors.Is(err, ErrGateInterrupted) {
			return err
		}
		if err == nil {
			stampLint(state, tree, g)
			return nil
		}
		var timedOut *GateTimeoutError
		p, env := ClassifyGateOutput(out)
		if env && !errors.As(err, &timedOut) {
			if attempt < gateEnvRetries {
				a.Store.Event(t.ID, EventGateEnv, fmt.Sprintf("done's lint gate: %s; retry %d of %d", p.Signature, attempt+1, gateEnvRetries))
				time.Sleep(time.Duration(attempt+1) * time.Second)
				continue
			}
			a.Store.Event(t.ID, EventGateEnv, "done's lint gate: "+p.Signature)
			return fmt.Errorf("done could not check your tree: the repo's gate `%s` failed on the environment (%q), not on your branch. Free %s, then call the saddle done tool again; your branch needs no change for it.%s\n%s",
				g.Cmd, p.Signature, p.Free, a.tempHint(), tail(out, 10))
		}
		if a.brokenGate(t.ID, g, out) {
			return nil
		}
		a.Store.Event(t.ID, "lint_failed", g.Cmd)
		return errors.New(lintFailure(g, out, "done refused: in your worktree"))
	}
}

// runLintGate runs the lint gate cmd in dir with TMPDIR and GOTMPDIR in a
// per-run dir under GateTmpdir, as the train's test gate does (#320), so a
// full /tmp doesn't block done when the train would have passed.
func (a *App) runLintGate(dir, cmd string) (string, error) {
	env, cleanup, out, err := gateTemp(a.GateTmpdir())
	if err != nil {
		return out, err
	}
	defer cleanup()
	return runGroup(context.Background(), dir, cmd, append(os.Environ(), env...), a.GateTimeout())
}

// hooksState is where the repo-hook wrapper keeps its lock and pass stamps:
// <git common dir>/saddle-hooks, shared by every worktree. "" if git can't
// say.
func hooksState(dir string) string {
	common, err := gitx.Run(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || common == "" {
		return ""
	}
	return filepath.Join(common, "saddle-hooks")
}

// cleanTree is the tree of dir's HEAD when nothing in dir differs from it,
// else "": an edited worktree isn't the tree a stamp vouches for.
func cleanTree(dir string) string {
	if st, err := gitx.Run(dir, "status", "--porcelain"); err != nil || st != "" {
		return ""
	}
	tree, err := gitx.Run(dir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return ""
	}
	return tree
}

// lintStamp is done's own pass stamp, beside the wrapper's <hook>.ok ones.
const lintStamp = "lint.ok"

// stampLine is the line lintStamp records for tree passing g.
func stampLine(tree string, g lintgate.Gate) string {
	sum := sha256.Sum256([]byte(g.Cmd))
	return tree + " " + hex.EncodeToString(sum[:8])
}

// passedLint reports whether tree already passed g: by done's own stamp, or
// for a gate saddle detected (the repo's pre-commit gate) by the stamp the
// repo-hook wrapper writes when its pre-commit hook passes.
func passedLint(state, tree string, g lintgate.Gate) bool {
	if state == "" || tree == "" {
		return false
	}
	if stamped(filepath.Join(state, lintStamp), stampLine(tree, g)) {
		return true
	}
	return g.Kind != "config" && stamped(filepath.Join(state, "pre-commit.ok"), tree)
}

func stamped(file, line string) bool {
	b, err := os.ReadFile(file)
	return err == nil && slices.Contains(strings.Split(string(b), "\n"), line)
}

// stampLint records that tree passed g, keeping the last 200.
func stampLint(state, tree string, g lintgate.Gate) {
	if state == "" || tree == "" || os.MkdirAll(state, 0o755) != nil {
		return
	}
	file := filepath.Join(state, lintStamp)
	b, _ := os.ReadFile(file)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	lines = append(lines, stampLine(tree, g))
	if len(lines) > 200 {
		lines = lines[len(lines)-200:]
	}
	tmp := fmt.Sprintf("%s.%d.tmp", file, os.Getpid())
	if os.WriteFile(tmp, []byte(strings.TrimSpace(strings.Join(lines, "\n"))+"\n"), 0o644) == nil {
		_ = os.Rename(tmp, file)
	}
}

// lockHooks takes the lock the repo-hook wrapper runs heavy hooks under: a
// flock on <state>/lock, or where flock(1) is missing, the <state>/lock.d
// dir holding its owner's pid. It waits up to max, then goes ahead without
// it rather than block done for good.
func lockHooks(state string, max time.Duration) (unlock func()) {
	noop := func() {}
	if state == "" || os.MkdirAll(state, 0o755) != nil {
		return noop
	}
	lock := filepath.Join(state, "lock")
	end := time.Now().Add(max)
	if _, err := exec.LookPath("flock"); err == nil {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return noop
		}
		for syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			if time.Now().After(end) {
				f.Close()
				return noop
			}
			time.Sleep(200 * time.Millisecond)
		}
		return func() { f.Close() }
	}
	dir := lock + ".d"
	for os.Mkdir(dir, 0o755) != nil {
		b, _ := os.ReadFile(filepath.Join(dir, "pid"))
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && !pidAlive(pid) {
			_ = os.RemoveAll(dir)
			continue
		}
		if time.Now().After(end) {
			return noop
		}
		time.Sleep(time.Second)
	}
	_ = os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
	return func() { _ = os.RemoveAll(dir) }
}

// trainLint runs the gate on the rebased tree in dir after the tests. It
// returns the failure message, "" when green, off, the same command the
// tests just ran (#270) or a tree that already passed it, and
// ErrGateInterrupted when saddle was told to stop.
func (a *App) trainLint(id, dir string) (string, error) {
	g := a.LintGate()
	if g.Cmd == "" || lintgate.SameCmd(g.Cmd, a.Cfg.Test.Cmd) {
		return "", nil
	}
	state, tree := hooksState(dir), cleanTree(dir)
	if passedLint(state, tree, g) {
		a.Store.Event(id, "lint_skipped", "tree "+tree+" already passed")
		return "", nil
	}
	out, err := a.runLintGate(dir, g.Cmd)
	if errors.Is(err, ErrGateInterrupted) {
		return "", err
	}
	if err == nil {
		stampLint(state, tree, g)
		return "", nil
	}
	if a.brokenGate(id, g, out) {
		return "", nil
	}
	return lintFailure(g, out, "Your branch rebased cleanly onto "+a.Cfg.Integration+" and passed its tests, but on the result"), nil
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
