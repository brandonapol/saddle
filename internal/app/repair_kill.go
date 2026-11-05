package app

import "github.com/brandonapol/saddle/internal/store"

// Who killed a task (#772). Saddle kills tasks too: the orphan finisher,
// rescue, the watchdog and the zombie sweep. Only the owner's kill says "this
// work isn't wanted", so only it declines a repair or spends a CI repair
// attempt. Saddle's own kills record eventKilledBy; a kill with no record is
// the owner's, as every kill was before saddle recorded them.

// eventKilledBy is recorded on a task saddle killed itself; data: the killer.
const eventKilledBy = "killed_by"

// The killers.
const (
	KilledByOwner    = "owner"
	KilledByOrphan   = "orphan"
	KilledByWatchdog = "watchdog"
	KilledByRescue   = "rescue"
	KilledByZombie   = "zombie"
)

// recordKiller notes that saddle, not the owner, killed task.
func (a *App) recordKiller(task, by string) { a.Store.Event(task, eventKilledBy, by) }

// killedBy names who killed task: KilledByOwner unless saddle recorded itself.
func (a *App) killedBy(task string) string {
	if by, ok := a.lastEvent(task, eventKilledBy); ok && by != "" {
		return by
	}
	return KilledByOwner
}

// ownerKilled reports whether t was killed, and by the owner.
func (a *App) ownerKilled(t store.Task) bool {
	return t.Status == store.Killed && a.killedBy(t.ID) == KilledByOwner
}

// internalKill reports whether task was killed by saddle itself.
func (a *App) internalKill(task string) bool { return a.killedBy(task) != KilledByOwner }
