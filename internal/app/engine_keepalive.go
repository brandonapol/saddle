package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Keeping the plugin engine alive outside the model (#789). `saddle plugin
// engine` writes .saddle/engine.want while it is meant to run, and removes
// it only when it is stopped on purpose: ctrl-c, `saddle down`, `saddle
// plugin engine --stop`, or saddle up taking the repo over. While the marker
// is there and nobody holds the engine lock, the engine died by accident,
// and the next MCP call or hook any agent makes starts it again, detached.

// Events the keep-alive records.
const (
	EventEngineRevived   = "engine_revived"
	EventEngineRestarted = "engine_restarted"
)

// EngineSupervisedEnv marks an engine its --supervise parent started: the
// parent owns the marker.
const EngineSupervisedEnv = "SADDLE_ENGINE_SUPERVISED"

// EngineReviveCooldown is the least time between two revivals, so a burst
// of tool calls, or an engine that dies at once, can't fork a storm.
var EngineReviveCooldown = 30 * time.Second

// StartEngine starts root's engine detached from the caller (its own
// session, so the agent's terminal going away doesn't take it down). Tests
// replace it.
var StartEngine = startEngineDetached

func (a *App) engineWantPath() string   { return a.stateDir("engine.want") }
func (a *App) engineRevivePath() string { return a.stateDir("engine.revive") }

// WantEngine records that the engine is meant to run, kept by process pid
// (the engine, or its --supervise parent).
func (a *App) WantEngine(pid int) error {
	if err := os.MkdirAll(a.stateDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(a.engineWantPath(), []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// EngineWanted reports whether the engine is meant to run, and the pid that
// keeps it (0 when unknown).
func (a *App) EngineWanted() (pid int, ok bool) {
	b, err := os.ReadFile(a.engineWantPath())
	if err != nil {
		return 0, false
	}
	pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	return pid, true
}

// UnwantEngine records that the engine was stopped on purpose, so nothing
// revives it, and returns the pid that kept it.
func (a *App) UnwantEngine() int {
	pid, _ := a.EngineWanted()
	_ = os.Remove(a.engineWantPath())
	return pid
}

// StopEngine stops this repo's plugin engine on purpose: the marker goes,
// then its supervisor and the engine get SIGINT, as from ctrl-c. It reports
// whether anything was signalled.
func (a *App) StopEngine() bool {
	pids := []int{a.UnwantEngine()}
	if a.LockOwner() == LockEngine {
		if b, err := os.ReadFile(a.lockPIDPath()); err == nil {
			pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
			pids = append(pids, pid)
		}
	}
	sent := false
	for _, pid := range slices.Compact(pids) {
		if pid > 0 && pid != os.Getpid() && pidAlive(pid) && syscall.Kill(pid, syscall.SIGINT) == nil {
			sent = true
		}
	}
	return sent
}

// ReviveEngine starts the engine when it is meant to run but nobody holds
// the engine lock, and reports whether it did. It is cheap when there is
// nothing to do (one stat), so the MCP server and the hook call it on every
// call. A live --supervise parent restarts its own child, so it is left to.
func (a *App) ReviveEngine() bool {
	keeper, ok := a.EngineWanted()
	if !ok || a.LockOwner() != "" {
		return false
	}
	if keeper > 0 && keeper != os.Getpid() && pidAlive(keeper) && !zombie(keeper) {
		if st, err := os.Stat(a.engineWantPath()); err == nil && !startedAfter(keeper, st.ModTime()) {
			return false // its supervisor is between restarts
		}
	}
	f, err := os.OpenFile(a.engineRevivePath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return false
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return false // another call is reviving it
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	if st, err := f.Stat(); err == nil && st.Size() > 0 && time.Since(st.ModTime()) < EngineReviveCooldown {
		return false
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt(fmt.Appendf(nil, "%d\n", time.Now().Unix()), 0)
	if err := StartEngine(a.Root); err != nil {
		a.Store.Event("", EventEngineRevived, "the engine was down; starting it again failed: "+err.Error())
		return false
	}
	a.Store.Event("", EventEngineRevived, "the engine was down but meant to run; an agent's call started it again")
	return true
}

func startEngineDetached(root string) error {
	if testing.Testing() {
		return errors.New("tests must set app.StartEngine")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	log, err := os.OpenFile(fmt.Sprintf("%s/.saddle/engine.log", root), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(exe, "plugin", "engine", "--supervise")
	cmd.Dir, cmd.Stdout, cmd.Stderr = root, log, log
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		// The engine is nobody's task, and its own supervisor.
		return strings.HasPrefix(kv, "SADDLE_TASK=") || strings.HasPrefix(kv, EngineSupervisedEnv+"=")
	})
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

// zombie reports whether process pid has exited but its parent has not
// reaped it yet: kill(pid, 0) still succeeds, but it runs nothing.
func zombie(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	i := strings.LastIndexByte(string(b), ')')
	f := strings.Fields(string(b[i+1:]))
	return i >= 0 && len(f) > 0 && (f[0] == "Z" || f[0] == "X")
}
