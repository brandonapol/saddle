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
