package app

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/store"
)

// The no-progress watchdog's automatic fixes (#783). A stall whose cause is
// a task saddle can see is broken (its worktree gone, its window gone, or
// given up on by the hung-worker watch) gets a card whose default clears it,
// instead of one whose default is to wait. Each action reads the tasks again
// when it runs, so a task that recovered meanwhile is left alone.

// stalledTasks lists the live workers the watchdog can blame by name: those
// whose worktree is gone, those with no window of their own, and those the
// hung-worker watch gave up on.
func (a *App) stalledTasks() (zombies, windowless, gaveUp []string) {
	ts, err := a.Store.Tasks()
	if err != nil {
		return nil, nil, nil
	}
	hung, _ := a.readHung()
	for _, t := range ts {
		if t.Role != store.RoleWorker || !liveStatus(t.Status) {
			continue
		}
		switch {
		case t.Worktree != "" && !isDir(t.Worktree):
			zombies = append(zombies, t.ID)
		case a.lost(t, false):
			windowless = append(windowless, t.ID)
		case hung[t.ID].Step == HungGaveUp:
			gaveUp = append(gaveUp, t.ID)
		}
	}
	return zombies, windowless, gaveUp
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// rescueStalled is the watchdog's default for zombies and given-up tasks:
// each is rescued (its work saved on rescue/<task>, then killed with its
// worktree kept), which frees its claims and its slot.
func (a *App) rescueStalled(DecisionCard, DecisionOption) error {
	zombies, _, gaveUp := a.stalledTasks()
	var done []string
	var errs []error
	for _, id := range append(zombies, gaveUp...) {
		r, err := a.Rescue(id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		a.Store.Event(id, "watchdog_rescued", r.Branch)
		done = append(done, id+" (on "+r.Branch+")")
	}
	if len(done) > 0 {
		errs = append(errs, a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
			"Nothing moved, so saddle rescued %s: each task's work is saved on its rescue branch and its slot is free.", strings.Join(done, ", "))))
	}
	return errors.Join(errs...)
}

// resumeStalled is the watchdog's default for tasks with no window: each
// gets a new one, as saddle resume would. A zombie is the rescue's, not
// this one's.
func (a *App) resumeStalled(DecisionCard, DecisionOption) error {
	zombies, _, _ := a.stalledTasks()
	_, err := a.resumeLost(false, func(t store.Task) bool { return !slices.Contains(zombies, t.ID) })
	return err
}
