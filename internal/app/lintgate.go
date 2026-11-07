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
//
// The wait for both locks is one wait, bounded by GateTimeout (#799). When
// it runs out the gate is not run unlocked: done defers the tree to the
// train's lint gate, which checks it before it lands.
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
	start, deadline, who := time.Now(), time.Now().Add(a.GateTimeout()), "done gate for "+t.ID
	queued := func(holder string) {
		msg := fmt.Sprintf("done's lint gate is queued behind %s; it waits up to %s", holder, a.GateTimeout())
		a.Store.Event(t.ID, EventLintQueued, msg)
		_ = a.Notify(t.ID, store.NoticeInfo, msg+". You aren't hung; keep working or wait.")
	}
	unlock, ok, holder := lockHooks(state, deadline, who, queued)
	defer unlock()
	if ok {
		var unlockLint func()
		unlockLint, ok, holder = lockNamed(state, lintLockName, deadline, who, queued)
		defer unlockLint()
	}
	if !ok {
		a.Store.Event(t.ID, EventLintDeferred, fmt.Sprintf(
			"waited %s for the gate lock held by %s; the train's lint gate checks this tree instead",
			time.Since(start).Round(time.Second), holder))
		return nil
	}
	if passedLint(state, tree, g) { // the hook we waited on passed it
		a.Store.Event(t.ID, "lint_skipped", "tree "+tree+" already passed")
		return nil
	}
	defer a.sweepScratchAfter(t.ID)
	retriedTimeout := false
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
		if errors.As(err, &timedOut) && !retriedTimeout {
			// A timeout says nothing about the tree (a loaded machine): run
			// the gate once more before bouncing the branch (#430).
			retriedTimeout = true
			a.Store.Event(t.ID, EventGateTimeoutRetry, "done's lint gate: "+err.Error()+"; retrying once")
			continue
		}
		p, env := ClassifyGateOutput(out)
		if env && !errors.As(err, &timedOut) {
			if attempt < gateEnvRetries {
				a.Store.Event(t.ID, EventGateEnv, fmt.Sprintf("done's lint gate: %s; retry %d of %d", p.Signature, attempt+1, gateEnvRetries))
				time.Sleep(time.Duration(attempt+1) * gateEnvPause)
				continue
			}
			a.Store.Event(t.ID, EventGateEnv, "done's lint gate: "+p.Signature)
			return &DoneEnvError{Cmd: g.Cmd, Problem: p, Out: out, Hint: a.tempHint()}
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

// stampKeep is how many pass stamps lintStamp keeps. 200 dropped stamps the
// train then re-gated (#800); a line is ~60 bytes, so this is ~120KB.
const stampKeep = 2000

// stampLint records that tree passed g, keeping the last stampKeep.
func stampLint(state, tree string, g lintgate.Gate) {
	if state == "" || tree == "" || os.MkdirAll(state, 0o755) != nil {
		return
	}
	file := filepath.Join(state, lintStamp)
	b, _ := os.ReadFile(file)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	lines = append(lines, stampLine(tree, g))
	if len(lines) > stampKeep {
		lines = lines[len(lines)-stampKeep:]
	}
	tmp := fmt.Sprintf("%s.%d.tmp", file, os.Getpid())
	if os.WriteFile(tmp, []byte(strings.TrimSpace(strings.Join(lines, "\n"))+"\n"), 0o644) == nil {
		_ = os.Rename(tmp, file)
	}
}

// Events of the gate locks (#799).
const (
	// EventLintQueued: done's lint gate found the lock held and waits.
	EventLintQueued = "lint_queued"
	// EventLintDeferred: done's lint gate gave up waiting and left the tree
	// to the train's lint gate rather than run it unlocked.
	EventLintDeferred = "lint_deferred"
)

// holderSuffix names the file beside a lock that says who holds it, so a
// waiter can say what it is queued behind. The repo-hook wrapper writes it
// too.
const holderSuffix = ".holder"

// lockHooks takes the lock the repo-hook wrapper runs heavy hooks under: a
// flock on <state>/lock, or where flock(1) is missing, the <state>/lock.d
// dir holding its owner's pid.
func lockHooks(state string, deadline time.Time, who string, queued func(holder string)) (unlock func(), ok bool, holder string) {
	return lockNamed(state, "lock", deadline, who, queued)
}

// lintLockName is the lock only lint runs take, so the train's lint step
// doesn't queue behind a done gate holding the hooks lock (#529) yet two lint
// runs still never overlap.
const lintLockName = "lint.lock"

// lockHolder is who <state>/<name>.holder says holds that lock.
func lockHolder(state, name string) string {
	b, _ := os.ReadFile(filepath.Join(state, name+holderSuffix))
	if h := strings.TrimSpace(string(b)); h != "" {
		return h
	}
	return "unknown"
}

// lockNamed takes the lock <state>/<name> for who, waiting until deadline.
// A held lock calls queued (if set) with its holder once, then waits blocked
// in the kernel, so it takes its turn beside the wrapper's flock(1) waiters
// instead of losing every race to them (#799). ok is false when the
// deadline passed first: the caller holds nothing and must not run as if it
// did. With no state dir or lock file there is nothing to serialize on, and
// ok is true.
func lockNamed(state, name string, deadline time.Time, who string, queued func(holder string)) (unlock func(), ok bool, holder string) {
	noop := func() {}
	if state == "" || os.MkdirAll(state, 0o755) != nil {
		return noop, true, ""
	}
	lock := filepath.Join(state, name)
	mark := func() {
		line := fmt.Sprintf("%s (pid %d, since %s)\n", who, os.Getpid(), time.Now().Format("15:04:05"))
		_ = os.WriteFile(lock+holderSuffix, []byte(line), 0o644)
	}
	if _, err := exec.LookPath("flock"); err == nil {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return noop, true, ""
		}
		fd := int(f.Fd())
		if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			holder = lockHolder(state, name)
			if queued != nil {
				queued(holder)
			}
			got := make(chan error, 1)
			go func() { got <- flockWait(fd) }()
			timer := time.NewTimer(time.Until(deadline))
			defer timer.Stop()
			select {
			case err := <-got:
				if err != nil {
					f.Close()
					return noop, false, holder
				}
			case <-timer.C:
				// The blocked flock can't be cancelled: let it finish,
				// then drop the lock it gets.
				go func() { <-got; f.Close() }()
				return noop, false, holder
			}
		}
		mark()
		return func() { f.Close() }, true, ""
	}
	dir := lock + ".d"
	for told := false; os.Mkdir(dir, 0o755) != nil; {
		b, _ := os.ReadFile(filepath.Join(dir, "pid"))
		if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && !pidAlive(pid) {
			_ = os.RemoveAll(dir)
			continue
		}
		holder = lockHolder(state, name)
		if !told && queued != nil {
			told = true
			queued(holder)
		}
		if time.Now().After(deadline) {
			return noop, false, holder
		}
		time.Sleep(time.Second)
	}
	_ = os.WriteFile(filepath.Join(dir, "pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
	mark()
	return func() { _ = os.RemoveAll(dir) }, true, ""
}

// flockWait blocks until it holds the exclusive flock on fd.
func flockWait(fd int) error {
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// trainLint runs the gate on the rebased tree in dir after the tests. Unlike
// lintDone it doesn't wait on the repo hooks' lock (a done gate can hold it for
// minutes, #529) but on a lint-only lock that done's lint also holds, and skips a tree that
// already passed, and like the test gate it runs through runGateEnv, so a
// machine failure (parallel golangci-lint, a full disk) is retried and,
// if it persists, returned as env rather than charged to the branch
// (#395). It returns the failure message, "" when green, off, the same
// command the tests just ran (#270) or a tree that already passed it, and
// ErrGateInterrupted when saddle was told to stop.
func (a *App) trainLint(id, dir string) (string, *GateEnvProblem, error) {
	g := a.LintGate()
	if g.Cmd == "" || lintgate.SameCmd(g.Cmd, a.Cfg.Test.Cmd) {
		return "", nil, nil
	}
	state, tree := hooksState(dir), cleanTree(dir)
	if passedLint(state, tree, g) {
		a.Store.Event(id, "lint_skipped", "tree "+tree+" already passed")
		return "", nil, nil
	}
	unlock, ok, holder := lockNamed(state, lintLockName, time.Now().Add(a.GateTimeout()), "merge train lint for "+id, nil)
	defer unlock()
	if !ok {
		// Never run unlocked (#799): a busy lock is the machine, not the
		// branch, so it is retried like any other env problem.
		return "", &GateEnvProblem{Signature: "the lint lock stayed held by " + holder,
			Free: "a lint run held the lock past the gate timeout; it frees when that run ends"}, nil
	}
	if passedLint(state, tree, g) { // the lint we waited on passed it
		a.Store.Event(id, "lint_skipped", "tree "+tree+" already passed")
		return "", nil, nil
	}
	r := a.RunGateEnv(context.Background(), id, ShellGate(dir, g.Cmd, a.GateTimeout()))
	if errors.Is(r.Err, ErrGateInterrupted) {
		return "", nil, r.Err
	}
	if r.Err == nil {
		stampLint(state, tree, g)
		return "", nil, nil
	}
	if r.Env != nil {
		return "", r.Env, nil
	}
	if a.brokenGate(id, g, r.Output) {
		return "", nil, nil
	}
	return lintFailure(g, r.Output, "Your branch rebased cleanly onto "+a.Cfg.Integration+" and passed its tests, but on the result"), nil, nil
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
