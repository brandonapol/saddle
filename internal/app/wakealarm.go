package app

import (
	"errors"
	"os"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// The wake alarm (#860). An idle orchestrator learns that an agent needs it
// through one of three wake paths: the saddle mod, a running `saddle plugin
// wait`, or its settings hook while the session is working. Each leaves a
// heartbeat. When all three are absent while workers are at work, nothing
// will deliver their notices, so the stall tick tells the owner, once per
// outage.

// EventWakeLost is logged when the wake alarm fires.
const EventWakeLost = "wake_lost"

// wakeAlarmMark starts the alarm's text.
const wakeAlarmMark = "Nothing can wake the orchestrator"

// wakeAlarmText is the alarm sent to t0 and the owner.
const wakeAlarmText = wakeAlarmMark + ": the saddle mod's heartbeat is stale, no `saddle plugin wait` is running and the session's hooks have been quiet for " +
	"over 10m, while workers are at work. Their notices wait undelivered. Open the orchestrating session and start `saddle plugin wait` in the background (run_in_background), or load the saddle mod."

func (a *App) wakeAlarmPath() string { return a.stateDir("wake-alarm") }

// WakeAlarmTick raises the wake alarm at now when every wake path is absent,
// a session has ever used one here, and a worker is at work. It reports
// whether it raised the alarm. A path coming back ends the outage, so the
// next one alarms again.
func (a *App) WakeAlarmTick(now time.Time) (bool, error) {
	if a.ModActive(now) || a.WaitActive(now) || a.HookActive(now) {
		if err := os.Remove(a.wakeAlarmPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		return false, nil
	}
	if !a.wakePathSeen() || !a.WorkerAtWork() {
		return false, nil
	}
	if _, err := os.Stat(a.wakeAlarmPath()); err == nil {
		return false, nil // this outage already alarmed
	}
	if err := os.WriteFile(a.wakeAlarmPath(), []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o644); err != nil {
		return false, err
	}
	a.Store.Event(OrchestratorID, EventWakeLost, "mod, wait and hook all absent")
	return true, a.tellOwner(wakeAlarmText)
}

// wakePathSeen reports whether any wake path ever left a heartbeat here: a
// repo driven only from the CLI or the TUI has no orchestrating session to
// wake.
func (a *App) wakePathSeen() bool {
	for _, p := range []string{a.ModHeartbeatPath(), a.WaitHeartbeatPath(), a.HookHeartbeatPath()} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// WorkerAtWork reports whether any worker is still at work.
func (a *App) WorkerAtWork() bool {
	ts, err := a.Store.Tasks()
	if err != nil {
		return false
	}
	for _, t := range ts {
		if t.Role == store.RoleWorker && t.Active() && t.Status != store.Done && t.Status != store.PROpen && t.Status != store.GaveUp {
			return true
		}
	}
	return false
}
