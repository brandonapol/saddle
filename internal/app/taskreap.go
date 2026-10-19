package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/procreap"
)

// A task's run dir (.saddle/run/<id>) holds its launch script, settings and
// adapter record. Kill and land reap whatever still runs under it and remove
// it (#676). Done keeps it, and so do a handoff and a hung takeover: the
// successor relaunches from the same dir. The worktree is gc's, and nothing
// is reaped by its cwd, since an owner's shell may sit in it.

// runKillGrace is how long a process gets after SIGTERM.
const runKillGrace = 2 * time.Second

// runMatch is how a run dir's processes are found.
var runMatch = procreap.Match{SkipTmux: true}

// layoutPrefix starts the name of a layout run's scratch worktree under
// .saddle (#354).
const layoutPrefix = "layout-"

// cleanRunDir reaps the processes under task id's run dir and removes it.
// A dir a process still holds after the reap is kept, with a note.
func (a *App) cleanRunDir(id string) {
	a.reapTask(id)
	dir := a.stateDir("run", id)
	if _, err := os.Stat(dir); err != nil {
		return
	}
	if a.runReap != nil {
		a.runReap(dir)
	} else {
		procreap.Reap(dir, runKillGrace, runMatch)
	}
	if pids := procreap.Find(dir, runMatch); len(pids) > 0 {
		a.Store.Event(id, "run_dir_kept", fmt.Sprintf("%s: still held by pid %v", dir, pids))
		return
	}
	if err := removeTree(dir); err != nil {
		a.Store.Event(id, "run_dir_kept", dir+": "+err.Error())
	}
}

// LayoutSweep is what SweepStaleLayouts removed.
type LayoutSweep struct {
	Dirs []string
}

// SweepStaleLayouts removes the layout-* worktrees under .saddle that a
// layout run which died midway left behind: untouched for maxAge and with no
// process working in one. Its git registration goes with it.
func (a *App) SweepStaleLayouts(maxAge time.Duration) LayoutSweep {
	var res LayoutSweep
	es, err := os.ReadDir(a.stateDir())
	if err != nil {
		return res
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range es {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), layoutPrefix) {
			continue
		}
		fi, err := e.Info()
		dir := filepath.Join(a.stateDir(), e.Name())
		if err != nil || !fi.ModTime().Before(cutoff) || len(procreap.Find(dir, runMatch)) > 0 {
			continue
		}
		_ = gitx.WorktreeRemove(a.Root, dir)
		err = removeTree(dir)
		if err == nil {
			err = a.forgetLayoutWorktree(dir)
		}
		if err != nil {
			a.Store.Event("", "layout_sweep_failed", dir+": "+err.Error())
			continue
		}
		res.Dirs = append(res.Dirs, dir)
	}
	if len(res.Dirs) > 0 {
		a.Store.Event("", "layout_swept", fmt.Sprintf("%d stale layout worktrees", len(res.Dirs)))
	}
	return res
}
