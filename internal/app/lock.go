package app

import (
	"errors"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Holders of the repo's engine lock. Only one process at a time drives the
// orchestrator: the TUI (saddle up), saddle up --rc, whose orchestrator is
// an interactive Claude Code session under Remote Control (#409), or the
// Claude Code plugin's engine.
const (
	LockUp     = "up"
	LockUpRC   = "up-rc"
	LockEngine = "engine"
)

// SessionWakes reports whether owner leaves the orchestrator's notices to
// an interactive Claude Code session, which takes them through the plugin
// paths (the mod, saddle plugin wait, the hooks), rather than to the TUI's
// headless process.
func SessionWakes(owner string) bool { return owner == LockEngine || owner == LockUpRC }

func (a *App) lockPath() string { return a.stateDir("tui.lock") }

// lockPIDPath names the engine lock holder's pid. It is a file of its own,
// not a second field in tui.lock, so an older saddle still reads the owner
// (#789).
func (a *App) lockPIDPath() string { return a.stateDir("tui.lock.pid") }

// AcquireLock takes the engine lock for owner and records the owner in the
// file, so LockOwner can tell the TUI from the plugin engine. The lock is
// held until release is called or the process exits.
func (a *App) AcquireLock(owner string) (release func(), err error) {
	if err := os.MkdirAll(a.stateDir(), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(a.lockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, lockHeldError(a.LockOwner())
	}
	if err := errors.Join(f.Truncate(0), writeAt0(f, owner+"\n")); err != nil {
		f.Close()
		return nil, err
	}
	_ = os.WriteFile(a.lockPIDPath(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644)
	// Unlock before closing: a process forked while the lock was held shares
	// the open file, and closing ours alone would leave it locked.
	return func() {
		_ = f.Truncate(0)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// flockHeldBy reports whether the flock on path is held and the file (or pidPath
// when set) names pid as its holder.
func flockHeldBy(path, pidPath string, pid int) bool {
	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		return false
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false
	}
	if pidPath == "" {
		pidPath = path
	}
	b, err := os.ReadFile(pidPath)
	if err != nil {
		return false
	}
	got, err := strconv.Atoi(strings.TrimSpace(string(b)))
	return err == nil && got == pid
}

// HoldsSessionLock reports whether process pid holds this repo's engine
// lock (saddle up, up --rc or the plugin engine) or saddle up's up.lock: the
// process that runs every watcher, which nothing else may stop (#789).
func (a *App) HoldsSessionLock(pid int) bool {
	return pid > 0 && (flockHeldBy(a.lockPath(), a.lockPIDPath(), pid) || flockHeldBy(a.stateDir("up.lock"), "", pid))
}

func writeAt0(f *os.File, s string) error {
	_, err := f.WriteAt([]byte(s), 0)
	return err
}

func lockHeldError(owner string) error {
	if owner == LockEngine {
		return errors.New("a Claude Code session is orchestrating this repo (saddle plugin engine is running); stop it first")
	}
	return errors.New("saddle up is already running for this repo in another terminal")
}

// LockOwner reports who holds the engine lock: LockUp, LockUpRC, LockEngine, or "" when
// nobody does. A lock held by an older saddle that wrote no owner reads as LockUp.
func (a *App) LockOwner() string {
	f, err := os.OpenFile(a.lockPath(), os.O_RDONLY, 0)
	if err != nil {
		return ""
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return ""
	}
	b, _ := os.ReadFile(a.lockPath())
	switch owner := strings.TrimSpace(string(b)); owner {
	case LockEngine, LockUpRC:
		return owner
	default:
		return LockUp
	}
}
