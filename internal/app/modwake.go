package app

import (
	"os"
	"time"
)

// ModFresh is how long the mod's heartbeat counts. The mod writes it every
// few seconds (#170), so a session that closed lets the settings hook and
// `saddle plugin wait` deliver events again within this long.
const ModFresh = 15 * time.Second

// ModHeartbeatPath is the file the saddle mod rewrites on each tick while it
// delivers the orchestrator's events in this repo.
func (a *App) ModHeartbeatPath() string { return a.stateDir("mod.alive") }

// ModActive reports whether a Claude Code session's saddle mod wakes the
// orchestrator here: its heartbeat was written within ModFresh of now. While
// it is, the settings hook and `saddle plugin wait` leave the notices to it,
// so no event is delivered twice.
func (a *App) ModActive(now time.Time) bool {
	fi, err := os.Stat(a.ModHeartbeatPath())
	if err != nil {
		return false
	}
	return now.Sub(fi.ModTime()) < ModFresh
}

// WaitFresh is how long a `saddle plugin wait` heartbeat counts. A running
// wait rewrites it every poll (about a second), so a wait that exited, or
// was never started, reads as absent within this long (#793).
const WaitFresh = 15 * time.Second

// WaitHeartbeatPath is the file a running `saddle plugin wait` rewrites.
func (a *App) WaitHeartbeatPath() string { return a.stateDir("wait.alive") }

// BeatWait records that a `saddle plugin wait` is running now.
func (a *App) BeatWait(now time.Time) {
	p := a.WaitHeartbeatPath()
	if err := os.WriteFile(p, []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o644); err == nil {
		_ = os.Chtimes(p, now, now)
	}
}

// WaitActive reports whether a `saddle plugin wait` is running: its
// heartbeat was written within WaitFresh of now.
func (a *App) WaitActive(now time.Time) bool {
	fi, err := os.Stat(a.WaitHeartbeatPath())
	if err != nil {
		return false
	}
	return now.Sub(fi.ModTime()) < WaitFresh
}

// WakeArmed reports whether something will wake an idle orchestrating
// session: the mod or a running `plugin wait`.
func (a *App) WakeArmed(now time.Time) bool { return a.ModActive(now) || a.WaitActive(now) }

// WaitInstruction is what a session with nothing left to wake it is told.
const WaitInstruction = "Nothing will wake you when an agent needs you: the saddle mod isn't running here and no `saddle plugin wait` is. Start one `saddle plugin wait` in the background (run_in_background) now, and start another each time it exits."

// EndWait clears the wait heartbeat as a wait exits, so the hooks see at
// once that nothing waits; another running wait rewrites it within a poll.
func (a *App) EndWait() { _ = os.Remove(a.WaitHeartbeatPath()) }
