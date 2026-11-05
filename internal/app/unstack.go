package app

import (
	"errors"
	"fmt"

	"github.com/brandonapol/saddle/internal/store"
)

// UnstackResult is what Unstack did to a task and its PR (#774).
type UnstackResult struct {
	Task   store.Task `json:"task"`
	State  string     `json:"state"`            // its train state before
	Closed bool       `json:"closed"`           // saddle closed its PR
	Note   string     `json:"note,omitempty"`   // what happened to its PR, in a sentence
	Repair string     `json:"repair,omitempty"` // the live repair it stopped, if any
}

// UnstackTask takes a task out of the PR stack for good, by task id, PR URL
// or PR number (#12), and says what happened to its PR. A stacked task
// leaves as superseded and its PR is left alone, as before. A repairing or
// escalated one is the case the repair notices point at (#774): it leaves
// as superseded too, its live repair is stopped, and its PR, already
// detached from the stack, is closed. A task already out is left as it is.
func (a *App) UnstackTask(ref string) (UnstackResult, error) {
	t, err := a.taskByRef(ref)
	res := UnstackResult{Task: t}
	if err != nil {
		return res, err
	}
	unlock, err := a.lockTrain()
	if err != nil {
		return res, err
	}
	defer unlock()
	e, ok := a.trainEntry(t.ID)
	if !ok {
		return res, fmt.Errorf("%s hasn't landed, so it isn't in the stack", t.ID)
	}
	res.State = e.State
	switch e.State {
	case store.TrainOK:
		l, ok := a.landedOne(t.ID)
		if !ok {
			return res, fmt.Errorf("%s hasn't landed, so it isn't in the stack", t.ID)
		}
		res.Note = prLeftOpen(t)
		return res, a.leaveStack(l, TrainSuperseded, "unstacked by hand")
	case TrainRepairing:
		l, ok := a.landedOne(t.ID)
		if !ok {
			return res, fmt.Errorf("%s's landed range is gone", t.ID)
		}
		res.Repair = a.stopRepair(t.ID)
		if err := a.leaveStack(l, TrainSuperseded, "unstacked by hand while repairing"); err != nil {
			return res, err
		}
	case TrainEscalated:
		if err := a.Store.SetTrain(t.ID, TrainSuperseded, "unstacked by hand while escalated: "+e.Note, false); err != nil {
			return res, err
		}
		a.Store.Event(t.ID, "unstacked", TrainSuperseded+": unstacked by hand while escalated")
		res.Repair = a.stopRepair(t.ID)
		if t.Active() {
			// Nobody will land it now: free its claims and its slot. Its
			// branch and worktree stay.
			if err := a.Kill(t.ID, true); err != nil {
				return res, err
			}
		}
	default:
		res.Note = "it was already out of the stack (" + e.State + "); nothing changed"
		return res, nil
	}
	res.Closed = a.closeDetached(t, fmt.Sprintf("Closed by saddle: %s was unstacked by hand while %s, so this PR won't merge.", t.ID, e.State))
	switch {
	case t.PR == "":
		res.Note = "it had no PR"
	case res.Closed:
		res.Note = "its PR " + t.PR + " was closed"
	default:
		res.Note = "closing its PR " + t.PR + " failed; close it by hand"
	}
	if res.Repair != "" {
		res.Note += "; its repair " + res.Repair + " was stopped"
	}
	return res, nil
}

// prLeftOpen says a stacked task's PR is left as it was.
func prLeftOpen(t store.Task) string {
	if t.PR == "" {
		return "it had no PR"
	}
	return "its PR " + t.PR + " is left open; close it if the work is abandoned"
}

// landedOne is task's landed entry.
func (a *App) landedOne(task string) (landedTask, bool) {
	all, err := a.landedAll()
	if err != nil {
		return landedTask{}, false
	}
	for _, l := range all {
		if l.ID == task {
			return l, true
		}
	}
	return landedTask{}, false
}

// stopRepair kills orig's live repair, the owner having dropped orig, and
// returns its id, "" if it had none.
func (a *App) stopRepair(orig string) string {
	r, ok := a.repairOf(orig)
	if !ok || !r.Active() {
		return ""
	}
	if err := a.Kill(r.ID, false); err != nil {
		a.Store.Event(orig, "unstack_error", errors.Join(fmt.Errorf("stop repair %s", r.ID), err).Error())
		return ""
	}
	return r.ID
}
