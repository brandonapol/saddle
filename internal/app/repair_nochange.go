package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/store"
)

// A repair that ends no_change never lands, so it never supersedes its
// original (#772). Left at that, the original stayed repairing forever: out
// of the stack with its PR open and nothing working on it. Now a no_change
// repair is dead, like a killed one, and saddle settles the original:
//   - base or integration already has the original's work (by patch-id):
//     the original leaves the stack as merged or superseded and its PR is
//     closed.
//   - otherwise the repair is respawned once, told what the last one said.
//   - a second no_change repair escalates: saddle spawns no more and tells
//     the orchestrator to repair by hand or unstack.

// repairNoChangeCap is how many no_change repairs end automatic repairs.
const repairNoChangeCap = 2

// repairDead reports a repair that will never land: killed, failed, or done
// with nothing to land.
func repairDead(t store.Task) bool {
	return t.Status == store.Killed || t.Status == store.NoChange || t.Status == StatusFailed
}

// repairsSpawned lists the repair tasks spawned for orig, oldest first.
func (a *App) repairsSpawned(orig string) []store.Task {
	es, err := a.Store.Events(-1)
	if err != nil {
		return nil
	}
	var out []store.Task
	for _, e := range es {
		if e.Task != orig || e.Kind != eventRepairSpawned {
			continue
		}
		if t, err := a.Store.Task(e.Data); err == nil {
			out = append(out, t)
		}
	}
	return out
}

// lastNoChangeRepair is orig's last repair when it ended no_change.
func (a *App) lastNoChangeRepair(orig string) (store.Task, bool) {
	rs := a.repairsSpawned(orig)
	if len(rs) == 0 || rs[len(rs)-1].Status != store.NoChange {
		return store.Task{}, false
	}
	return rs[len(rs)-1], true
}

// noChangeRepairs counts orig's repairs that ended no_change.
func (a *App) noChangeRepairs(orig string) int {
	n := 0
	for _, r := range a.repairsSpawned(orig) {
		if r.Status == store.NoChange {
			n++
		}
	}
	return n
}

// noChangeBlock is why saddle spawns no more repairs for orig after its
// no_change ones, "" when it may.
func (a *App) noChangeBlock(orig string) string {
	n := a.noChangeRepairs(orig)
	if n < repairNoChangeCap {
		return ""
	}
	r, _ := a.lastNoChangeRepair(orig)
	return fmt.Sprintf("%s's last %d repairs ended with nothing to land (the last, %s, said: %s), yet neither %s nor %s has its work, so saddle won't spawn another. "+
		"%s stays out of the stack with its PR open. Run `saddle repair %s` to try again by hand, or `saddle unstack %s` to drop it and close its PR.",
		orig, n, r.ID, oneLine(orNone(r.Summary)), a.Cfg.Base, a.Cfg.Integration, orig, orig, orig)
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(no summary)"
	}
	return s
}

// settleNoChange settles repairing task l whose last repair ended no_change
// when base or integration already has its work: it leaves the stack and
// its PR is closed. It reports whether it did.
func (a *App) settleNoChange(l landedTask) bool {
	if l.State != TrainRepairing || l.From == "" || l.To == "" {
		return false
	}
	r, ok := a.lastNoChangeRepair(l.ID)
	if !ok {
		return false
	}
	state, where := "", ""
	switch {
	case !lacks(a.Root, "refs/remotes/origin/"+a.Cfg.Base, l.From, l.To):
		state, where = TrainMerged, a.Cfg.Base
	case !lacks(a.Root, a.Cfg.Integration, l.From, l.To):
		state, where = TrainSuperseded, a.Cfg.Integration
	default:
		return false
	}
	why := fmt.Sprintf("its repair %s ended no_change and %s already has its work", r.ID, where)
	if err := a.leaveStack(l, state, why); err != nil {
		a.Store.Event(l.ID, eventRepairFailed, "settle no_change: "+err.Error())
		return false
	}
	a.closeDetached(l.Task, fmt.Sprintf("Closed by saddle: %s's repair %s found nothing to land because %s already has this PR's work.", l.ID, r.ID, where))
	return true
}

// closeDetached comments on and closes the PR of t, which left the stack
// for good, recording any failure. It reports whether the PR was closed.
func (a *App) closeDetached(t store.Task, body string) bool {
	if t.PR == "" {
		return false
	}
	if err := a.Forge().AddComment(context.Background(), t.PR, body); err != nil {
		a.Store.Event(t.ID, "pr_close_error", err.Error())
	}
	if err := a.Forge().ClosePR(context.Background(), t.PR); err != nil {
		a.Store.Event(t.ID, "pr_close_error", err.Error())
		return false
	}
	a.Store.Event(t.ID, "pr_closed", t.PR)
	return true
}

// settleRepairing settles orig if it is repairing after a no_change repair
// whose work is already in place.
func (a *App) settleRepairing(orig string) bool {
	all, err := a.landedAll()
	if err != nil {
		return false
	}
	for _, l := range all {
		if l.ID == orig {
			return a.settleNoChange(l)
		}
	}
	return false
}

// noChangeNote tells a respawned repair what its no_change predecessor said.
func (a *App) noChangeNote(orig store.Task) string {
	r, ok := a.lastNoChangeRepair(orig.ID)
	if !ok {
		return ""
	}
	return fmt.Sprintf("A previous repair, %s, finished with nothing to land. It said:\n%s\n"+
		"Neither %s nor %s has %s's work by patch-id, so check again before you conclude the same. "+
		"If the work really is there in another form, say exactly where in your summary.\n\n",
		r.ID, strings.TrimSpace(orNone(r.Summary)), a.Cfg.Base, a.Cfg.Integration, orig.ID)
}
