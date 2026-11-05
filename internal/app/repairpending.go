package app

import (
	"fmt"
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/claims"
	"github.com/brandonapol/saddle/internal/store"
)

// Pending repairs (#768, #795). A repair saddle owes but couldn't spawn
// (the concurrency cap, a claim overlap) used to wait for the event that
// first triggered it: a new red head, or a restack that got all the way
// through. Meanwhile fresh tickets took the free slots and the hot files.
// Now every such repair is pending until it spawns:
//   - RetryRepairs tries each again; the sentinel's StallTick runs it every
//     cycle, and autopilot runs it before each ticket.
//   - a pending repair that waits on the cap goes first: autopilot spawns no
//     ticket while one does.
//   - its files are reserved: a fresh spawn that overlaps them is refused,
//     and the original's own claims stay put until the repair takes them.
//
// Each kind of repair lists its pending repairs (pendingLists) and knows how
// to retry one (retryRepair). A new kind (gate reds, #773) adds to both.

// PendingRepair is a repair saddle owes but hasn't spawned yet.
type PendingRepair struct {
	Kind   string   `json:"kind"`             // ci, restack or land
	Task   string   `json:"task"`             // the task it repairs
	Claims []string `json:"claims,omitempty"` // the files it reserves
	Err    string   `json:"err,omitempty"`    // why its last spawn failed, "" if none tried yet
}

// AtCap reports whether p's last spawn failed for want of a slot: it goes
// before new tickets.
func (p PendingRepair) AtCap() bool {
	return strings.Contains(p.Err, "at concurrency cap") || strings.Contains(p.Err, ErrBackpressure.Error())
}

// pendingLists list every kind of repair that can be pending; retryRepair
// retries one by its Kind. A new kind adds to both. Spawn reads the lists
// (reservations), so they must never spawn.
func pendingLists() []func(*App) []PendingRepair {
	return []func(*App) []PendingRepair{(*App).ciPendingRepairs, (*App).restackPendingRepairs, (*App).landPendingRepairs}
}

// retryRepair tries to spawn p and returns the repair task's id, "" when
// it didn't spawn one.
func (a *App) retryRepair(p PendingRepair) (string, error) {
	switch p.Kind {
	case "ci":
		return a.retryCIRepair(p)
	case "restack", "land":
		return a.retryConflictRepair(p)
	}
	return "", fmt.Errorf("unknown pending repair kind %q", p.Kind)
}

// PendingRepairs lists every repair saddle owes and hasn't spawned.
func (a *App) PendingRepairs() []PendingRepair {
	var out []PendingRepair
	for _, l := range pendingLists() {
		out = append(out, l(a)...)
	}
	return out
}

// RetryRepairs tries to spawn every pending repair and returns the repair
// tasks it spawned.
func (a *App) RetryRepairs() []string {
	var out []string
	for _, p := range a.PendingRepairs() {
		id, err := a.retryRepair(p)
		if err != nil {
			a.Store.Event(p.Task, "repair_retry_error", p.Kind+": "+err.Error())
			continue
		}
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// repairFirst is the error a fresh ticket gets while a repair waits on the
// cap: the repair takes the next slot (#768). It is nil when none waits.
func (a *App) repairFirst() error {
	for _, p := range a.PendingRepairs() {
		if p.AtCap() {
			return fmt.Errorf("at concurrency cap: %s's %s repair is pending and takes the next free slot before new tickets (#768)", p.Task, p.Kind)
		}
	}
	return nil
}

// reservedErr refuses claims that overlap a pending repair's reserved
// files, unless the spawn is that repair (forTask). The original's own live
// claims conflict as claims; this covers what a landed original no longer
// holds (#795).
func (a *App) reservedErr(want []string, forTask string) error {
	var lines, tasks []string
	for _, p := range a.PendingRepairs() {
		if p.Task == forTask {
			continue
		}
		for _, w := range want {
			if i := slices.IndexFunc(p.Claims, func(g string) bool { return claims.Overlap(w, g) }); i >= 0 {
				lines = append(lines, fmt.Sprintf("%s overlaps %s, reserved for %s's pending %s repair", w, p.Claims[i], p.Task, p.Kind))
				if !slices.Contains(tasks, p.Task) {
					tasks = append(tasks, p.Task)
				}
			}
		}
	}
	if len(lines) == 0 {
		return nil
	}
	return fmt.Errorf("claim conflict: %s. The repair spawns as soon as it can and then holds these files; "+
		"wait for it, spawn with claims it doesn't reserve, run `saddle repair %s --force` to spawn it now, or `saddle unstack %s` to drop it",
		strings.Join(lines, "; "), tasks[0], tasks[0])
}

// ciPendingRepairs are the red layers whose CI repair is pending on the
// head they are still red on.
func (a *App) ciPendingRepairs() []PendingRepair {
	s, err := a.CIRed()
	if err != nil {
		return nil
	}
	var out []PendingRepair
	for _, l := range s.Red {
		for _, r := range s.Repairs {
			if r.Task == l.Task && r.Head == l.Head && r.State == ciRepairPending {
				out = append(out, PendingRepair{Kind: "ci", Task: l.Task, Claims: r.Claims, Err: r.Note})
			}
		}
	}
	return out
}

// CIRepairPending reports whether task's CI repair is pending on the head
// it is red on.
func (a *App) CIRepairPending(task string) bool {
	return slices.ContainsFunc(a.ciPendingRepairs(), func(p PendingRepair) bool { return p.Task == task })
}

// retryCIRepair runs CIRedRepair again on the failures it recorded.
func (a *App) retryCIRepair(p PendingRepair) (string, error) {
	note, err := a.CIRedRepair(p.Task, nil)
	if err != nil {
		return "", err
	}
	s, err := a.CIRed()
	if err != nil {
		return "", err
	}
	r, ok := a.liveCIRepair(s, p.Task)
	if !ok {
		return "", nil
	}
	return r, a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf("%s's pending CI repair spawned. %s", p.Task, note))
}

// restackPendingRepairs are the repairing tasks with no repair task.
func (a *App) restackPendingRepairs() []PendingRepair {
	all, err := a.landedAll()
	if err != nil {
		return nil
	}
	var out []PendingRepair
	for _, l := range all {
		if l.State != TrainRepairing {
			continue
		}
		if p, ok := a.conflictPending("restack", l.Task); ok {
			out = append(out, p)
		}
	}
	return out
}

// landPendingRepairs are the branches whose land conflicted with their agent
// gone, and whose repair couldn't spawn. Their claims are still theirs, so
// they reserve nothing more.
func (a *App) landPendingRepairs() []PendingRepair {
	es, err := a.Store.Train()
	if err != nil {
		return nil
	}
	var out []PendingRepair
	for _, e := range es {
		if e.State != store.TrainError && e.State != TrainEscalated && e.State != store.TestFailed {
			continue
		}
		t, err := a.Store.Task(e.Task)
		if err != nil || !t.Active() || !a.orphaned(t.ID) {
			continue
		}
		if s, ok := a.seedOf(t.ID); !ok || s.Source != "land" {
			continue
		}
		if p, ok := a.conflictPending("land", t); ok {
			p.Claims = nil
			out = append(out, p)
		}
	}
	return out
}

// conflictPending is orig's pending conflict repair, if it has no live or
// landed repair and nothing stops an automatic one.
func (a *App) conflictPending(kind string, orig store.Task) (PendingRepair, bool) {
	if _, live := a.repairOf(orig.ID); live || a.autoRepairBlock(orig.ID) != "" {
		return PendingRepair{}, false
	}
	seed, _ := a.seedOf(orig.ID)
	p := PendingRepair{Kind: kind, Task: orig.ID, Claims: a.repairClaims(orig, seed)}
	p.Err = a.lastRepairFailure(orig.ID)
	return p, true
}

// lastRepairFailure is why orig's last repair spawn failed, "" when its last
// repair event isn't a failure.
func (a *App) lastRepairFailure(orig string) string {
	es, err := a.Store.Events(-1)
	if err != nil {
		return ""
	}
	for i := len(es) - 1; i >= 0; i-- {
		if e := es[i]; e.Task == orig {
			switch e.Kind {
			case eventRepairFailed:
				return e.Data
			case eventRepairSpawned:
				return ""
			}
		}
	}
	return ""
}

// retryConflictRepair spawns a conflict repair from its recorded seed.
func (a *App) retryConflictRepair(p PendingRepair) (string, error) {
	t, err := a.Store.Task(p.Task)
	if err != nil {
		return "", err
	}
	if p.Kind == "restack" && a.settleRepairing(t.ID) {
		return "", nil // its no_change repair found its work in place (#772)
	}
	seed, _ := a.seedOf(t.ID)
	r, created, err := a.ensureRepair(t, seed, false)
	if err != nil || !created {
		return "", nil // recorded and notified once per reason by ensureRepair
	}
	return r.ID, nil
}
