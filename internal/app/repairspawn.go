package app

import (
	"fmt"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// Restack spawns a repair for every repairing task with none live (#172).
// When the owner kills that repair, the work is usually superseded by
// newer work, and respawning it on every restack wastes tokens and makes
// the owner kill it again (#337). A killed repair is a "do not repair"
// mark: restack leaves the task alone and says so once, and `saddle
// repair` spawns one by hand. Repairs per task are also capped per day.

// eventRepairDeclined is recorded on the original when restack skips its
// repair; data: the killed repair's id, or the cap.
const eventRepairDeclined = "repair_declined"

// repairDailyCap is how many repairs restack spawns for one task in a day.
const repairDailyCap = 2

// repairDeclined reports the repair of orig the owner killed: the last
// repair spawned for it, when that one is killed.
func (a *App) repairDeclined(orig string) (string, bool) {
	r, ok := a.lastEvent(orig, eventRepairSpawned)
	if !ok {
		return "", false
	}
	t, err := a.Store.Task(r)
	return r, err == nil && t.Status == store.Killed
}

// repairsToday counts the repairs spawned for task in the day before now.
func repairsToday(es []store.Event, task string, now time.Time) int {
	n := 0
	for _, e := range es {
		if e.Task == task && e.Kind == eventRepairSpawned && now.Sub(e.TS) < 24*time.Hour {
			n++
		}
	}
	return n
}

// skipAutoRepair reports whether restack must not spawn orig's repair: its
// last one was killed, or the day's cap is spent. The orchestrator hears
// once per reason.
func (a *App) skipAutoRepair(orig string) bool {
	why := ""
	if r, ok := a.repairDeclined(orig); ok {
		why = fmt.Sprintf("%s's repair %s was killed, so restack won't spawn another: its work looks superseded. "+
			"%s stays out of the stack and its PR is left alone. Run `saddle repair %s` if it still needs re-landing, or `saddle unstack %s` to drop it.",
			orig, r, orig, orig, orig)
	} else if es, err := a.Store.Events(-1); err == nil && repairsToday(es, orig, time.Now()) >= repairDailyCap {
		why = fmt.Sprintf("%s already had %d repairs in the last day, so restack won't spawn another until tomorrow. "+
			"Run `saddle repair %s` to spawn one now.", orig, repairDailyCap, orig)
	}
	if why == "" {
		return false
	}
	if last, ok := a.lastEvent(orig, eventRepairDeclined); !ok || last != why {
		a.Store.Event(orig, eventRepairDeclined, why)
		_ = a.Notify(OrchestratorID, store.NoticeInfo, why)
	}
	return true
}
