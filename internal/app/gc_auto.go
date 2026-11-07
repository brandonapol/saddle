package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/gc"
	"github.com/brandonapol/saddle/internal/store"
)

// Automatic gc (#802): the engine and saddle up run what saddle gc does on a
// timer, so worktrees, branches, backup refs, loose objects and old event
// rows stop piling up between manual runs.

// GCInterval is how often the gc watcher runs.
const GCInterval = 30 * time.Minute

// gcFirstDelay keeps the first run off saddle up's startup.
const gcFirstDelay = 2 * time.Minute

// EventGCAuto records one automatic gc run: what it removed, or why it
// skipped, or what failed.
const EventGCAuto = "gc_auto"

// AutoGCReport is what one automatic gc run did.
type AutoGCReport struct {
	Skipped string // why nothing ran, "" when it did
	Removed int    // worktrees, branches and refs removed
	Kept    int    // leftovers kept, each with its reason
	Backups int    // integration backups pruned
	Scratch int    // stale layout and ci-fix worktrees swept
	Rows    gc.DBResult
}

func (r AutoGCReport) String() string {
	if r.Skipped != "" {
		return "skipped: " + r.Skipped
	}
	return fmt.Sprintf("removed %d, kept %d, %d backups, %d scratch worktrees, %d events, %d notices, %d chat rows",
		r.Removed, r.Kept, r.Backups, r.Scratch, r.Rows.Events, r.Rows.Notices, r.Rows.Chat)
}

// AutoGC is one automatic gc run, recorded as a gc_auto event. It skips while the train holds its lock,
// and holds the lock itself so nothing lands or restacks under it. Unlike
// saddle gc it leaves anything not plainly a dead task's to the manual run
// (autoKeep). A failed step is reported and the rest still run. now dates
// the row retention.
func (a *App) AutoGC(now time.Time) (r AutoGCReport, err error) {
	defer func() {
		msg := r.String()
		if err != nil {
			msg += "; errors: " + err.Error()
		}
		a.Store.Event("", EventGCAuto, msg)
	}()
	unlock, ok, err := a.TryLockTrain()
	if err != nil {
		return r, err
	}
	if !ok {
		r.Skipped = "the train is busy"
		return r, nil
	}
	defer unlock()
	var errs []error
	a.gcFetch()
	var ls []Leftover
	ls, err = a.Leftovers()
	if err == nil {
		ls, err = a.removeLeftovers(ls, a.autoKeep)
	}
	errs = append(errs, err)
	for _, l := range ls {
		if l.Keep != "" {
			r.Kept++
		} else {
			r.Removed++
		}
	}
	gone, err := gc.PruneBackups(a.Root, gc.BackupPrefix, gc.BackupKeep)
	r.Backups = len(gone)
	errs = append(errs, err)
	r.Scratch = len(a.SweepStaleLayouts(GateSweepAge).Dirs)
	r.Rows, err = gc.PruneDB(a.stateDir("state.db"), now, gc.Retention{})
	errs = append(errs, err, gc.Maintain(a.Root))
	return r, errors.Join(errs...)
}

// autoKeep is automatic gc's last look at a leftover before it goes: only a
// task the store knows, and that is dead right now, loses its things. A name
// no task owns, or a task the store doesn't know (a spawn racing the run, a
// reset state.db), waits for a manual saddle gc.
func (a *App) autoKeep(l Leftover) string {
	name := filepath.Base(l.Name)
	if l.Kind == "branch" || l.Kind == "remote" {
		_, name, _ = strings.Cut(l.Name, "saddle/")
	}
	id := taskIDRe.FindString(name)
	if id == "" {
		return "not a task's; run saddle gc"
	}
	t, err := a.Store.Task(id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return "task " + id + " unknown; run saddle gc"
	case err != nil:
		return err.Error()
	case t.Active() && t.Status != StatusFailed:
		return "task " + id + " is live"
	}
	return ""
}

// RunGC is the supervised gc watcher (#789): an automatic gc every
// GCInterval, the first a little after start.
func (a *App) RunGC(ctx context.Context) { a.runGC(ctx, gcFirstDelay, GCInterval) }

func (a *App) runGC(ctx context.Context, first, every time.Duration) {
	wait := first
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = every
		_, _ = a.AutoGC(time.Now()) // it records what it did
	}
}
