package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/scratch"
	"github.com/brandonapol/saddle/internal/store"
)

// Saddle keeps all of its scratch under one root (#322, docs/scratch.md):
// [tmp] dir, else <user cache dir>/saddle/tmp. The MCP server and saddle up
// point their TMPDIR and GOTMPDIR at it at start, so the train, the gates
// and every agent spawned from them inherit it; the gates' per-run dirs sit
// in a repo dir under it. Saddle sweeps it on up, on a timer while up runs,
// and after a task's done or kill, and holds spawns while its filesystem
// stays low on space.

// EventScratchSwept records a sweep that removed something.
const EventScratchSwept = "scratch_swept"

// EventScratchLow records a spawn held for low scratch space.
const EventScratchLow = "scratch_low"

// ErrScratchLow is spawn's refusal while the scratch filesystem stays low
// on space after a sweep.
var ErrScratchLow = errors.New("scratch space low")

// ScratchSweepEvery is how often saddle up's sweeper runs.
const ScratchSweepEvery = 10 * time.Minute

// ScratchConfig is [tmp], read from the config files.
func (a *App) ScratchConfig() scratch.Config { return scratch.LoadConfig(a.Root) }

// ScratchRoot is the scratch root. A [tmp] dir inside the repo is ignored,
// for the reason GateTmpdir gives.
func (a *App) ScratchRoot() string {
	c := a.ScratchConfig()
	if r := c.Root(); !within(a.Root, r) {
		return r
	}
	return scratch.DefaultRoot()
}

// UseScratch points this process's TMPDIR and GOTMPDIR at the scratch root,
// so what saddle starts from here on writes its temp files there.
func (a *App) UseScratch() error { return scratch.Use(a.ScratchRoot()) }

// scratchOptions are the sweep's options for this repo.
func (a *App) scratchOptions(dry bool, maxAge time.Duration) scratch.Options {
	if maxAge <= 0 {
		maxAge = a.ScratchConfig().Age()
	}
	return scratch.Options{Root: a.ScratchRoot(), OSTemp: scratch.OSTemp(), MaxAge: maxAge, DryRun: dry}
}

// SweepScratch sweeps saddle's stale scratch now (scratch.Sweep) and
// records what it removed. maxAge 0 is [tmp] max_age.
func (a *App) SweepScratch(dry bool, maxAge time.Duration) scratch.Result {
	res := scratch.Sweep(a.scratchOptions(dry, maxAge))
	if !dry && len(res.Removed) > 0 {
		a.Store.Event("", EventScratchSwept, fmt.Sprintf("%d entries, %s", len(res.Removed), scratch.Bytes(res.Freed)))
	}
	return res
}

// sweeping is set while a background sweep runs, so they never pile up.
var sweeping atomic.Bool

// sweepScratchAfter sweeps in the background after task's done or kill.
// Tests skip it: it would sweep the machine's scratch, not theirs.
func (a *App) sweepScratchAfter(task string) {
	if testing.Testing() || !sweeping.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer sweeping.Store(false)
		res := scratch.Sweep(a.scratchOptions(false, 0))
		if len(res.Removed) > 0 {
			a.Store.Event(task, EventScratchSwept, fmt.Sprintf("after %s: %d entries, %s", task, len(res.Removed), scratch.Bytes(res.Freed)))
		}
	}()
}

// ScratchPressure checks the scratch filesystem and, when it is low, sweeps
// harder (scratch.Respond). space nil is the real filesystem.
func (a *App) ScratchPressure(space scratch.SpaceFunc) (scratch.Pressure, error) {
	p, err := scratch.Respond(a.scratchOptions(false, 0), a.ScratchConfig().LowFree(), space)
	if p.Swept != nil && len(p.Swept.Removed) > 0 {
		a.Store.Event("", EventScratchSwept, fmt.Sprintf("low space: %d entries, %s", len(p.Swept.Removed), scratch.Bytes(p.Swept.Freed)))
	}
	return p, err
}

// scratchSpace is the filesystem check spawn and the sweeper use; tests
// replace it.
var scratchSpace scratch.SpaceFunc = scratch.Statfs

// scratchHeld remembers, per scratch root, whether spawns are on hold, so
// the orchestrator hears once when the hold starts and once when it ends.
var scratchHeld = struct {
	sync.Mutex
	roots map[string]bool
}{roots: map[string]bool{}}

// setScratchHeld records the hold state and reports whether it changed.
func setScratchHeld(root string, held bool) bool {
	scratchHeld.Lock()
	defer scratchHeld.Unlock()
	if scratchHeld.roots[root] == held {
		return false
	}
	scratchHeld.roots[root] = held
	return true
}

// checkScratch refuses a spawn while the scratch filesystem stays low after
// a harder sweep, and tells the orchestrator what uses the space. A check
// that fails to measure lets the spawn through. Spawn's Force skips it.
func (a *App) checkScratch() error {
	p, err := a.ScratchPressure(scratchSpace)
	if err != nil {
		return nil
	}
	a.noteScratch(p)
	if !p.Low() {
		return nil
	}
	return fmt.Errorf("%w: %s New agents would fail their gates on it; spawn again once there is room, or override with spawn's force", ErrScratchLow, p.Message())
}

// noteScratch records a change in the hold for p's root and tells the
// orchestrator: the banner with what uses the space, or that spawns resumed.
func (a *App) noteScratch(p scratch.Pressure) {
	root := p.After.Path
	if !setScratchHeld(root, p.Low()) {
		return
	}
	if p.Low() {
		a.Store.Event(OrchestratorID, EventScratchLow, p.After.String())
		_ = a.Notify(OrchestratorID, store.NoticeAction, "Spawns are on hold: "+p.Message())
		return
	}
	_ = a.Notify(OrchestratorID, store.NoticeInfo, "Scratch has room again ("+p.After.String()+"); spawns resume.")
}

// RunScratchSweeper sweeps at start and every ScratchSweepEvery until ctx
// ends: the pressure check first, then the normal sweep. Failures are
// events.
func (a *App) RunScratchSweeper(ctx context.Context) {
	for {
		if p, err := a.ScratchPressure(scratchSpace); err == nil {
			a.noteScratch(p)
		}
		a.SweepScratch(false, 0)
		select {
		case <-ctx.Done():
			return
		case <-time.After(ScratchSweepEvery):
		}
	}
}

// ScratchWarning is status's line when the scratch filesystem is low, ""
// otherwise. It only measures, so status stays cheap.
func (a *App) ScratchWarning() string {
	root := a.ScratchRoot()
	th := a.ScratchConfig().LowFree()
	s, err := scratchSpace(root)
	if err != nil || th <= 0 || s.FreePct() >= th {
		return ""
	}
	return fmt.Sprintf("scratch is low on space: %s, under %.0f%%; new spawns are held. `saddle doctor` shows what uses it, `saddle gc` sweeps saddle's own.", s, th)
}

// tempHint names the temp dirs' entries with the most files, for a gate
// that failed on temp space (#320): a leak of empty dirs shows up at once.
func (a *App) tempHint() string {
	var top []scratch.Entry
	if root := a.ScratchRoot(); dirExists(root) {
		top = append(top, scratch.Top(root, 3, true, true)...)
	}
	if t := scratch.OSTemp(); dirExists(t) && filepath.Clean(t) != filepath.Clean(a.ScratchRoot()) {
		top = append(top, scratch.Top(t, 3, true, false)...)
	}
	if len(top) == 0 {
		return ""
	}
	return " Most files: " + scratch.Describe(top) + "."
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
