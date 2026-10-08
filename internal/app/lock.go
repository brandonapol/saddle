package app

import (
	"errors"
	"os"
	"strings"
	"syscall"
)

// Holders of the repo's engine lock. Only one process at a time drives the
// orchestrator: the TUI (saddle up) or the Claude Code plugin's engine.
const (
	LockUp     = "up"
	LockEngine = "engine"
)

func (a *App) lockPath() string { return a.stateDir("tui.lock") }

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
	// Unlock before closing: a process forked while the lock was held shares
	// the open file, and closing ours alone would leave it locked.
	return func() {
		_ = f.Truncate(0)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
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

// LockOwner reports who holds the engine lock: LockUp, LockEngine, or "" when
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
	case LockEngine:
		return owner
	default:
		return LockUp
	}
}
