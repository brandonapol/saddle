package app

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/brandonapol/saddle/internal/scratch"
	"github.com/brandonapol/saddle/internal/store"
)

// Each spawned agent gets its own TMPDIR on disk (#327):
// <scratch root>/<repo hash>/task-<id>. Its window starts with TMPDIR,
// GOTMPDIR and CLAUDE_CODE_TMPDIR there, so what the agent and its tools
// write to the temp dir neither lands in a tmpfs /tmp nor outlives the
// task. Kill and land remove it. Done keeps it: the agent may still be sent
// back to fix its branch. The sweep never removes a live task's dir, and
// removes a dead or unknown task's dir however fresh it is.

// taskTmpPrefix starts a task's tmp dir name.
const taskTmpPrefix = "task-"

// taskTmpEnv are the variables pointed at a task's tmp dir.
var taskTmpEnv = []string{"TMPDIR", "GOTMPDIR", "CLAUDE_CODE_TMPDIR"}

// taskTmpParent is the dir every task's tmp dir sits in.
func (a *App) taskTmpParent() string {
	return filepath.Join(a.ScratchRoot(), scratch.RepoKey(a.Root))
}

// taskTmp is task id's tmp dir.
func (a *App) taskTmp(id string) string {
	return filepath.Join(a.taskTmpParent(), taskTmpPrefix+id)
}

// taskTmpCmd creates task id's tmp dir and returns cmd started with the
// temp variables pointed at it. A dir that can't be made leaves cmd as it
// is: the agent then inherits saddle's own TMPDIR, the scratch root.
func (a *App) taskTmpCmd(id, cmd string) string {
	dir := a.taskTmp(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		a.Store.Event(id, "task_tmp_failed", err.Error())
		return cmd
	}
	var b strings.Builder
	b.WriteString("env")
	for _, k := range taskTmpEnv {
		b.WriteString(" " + k + "=" + shQuote(dir))
	}
	return b.String() + " " + cmd
}

// removeTaskTmp removes task id's tmp dir, once its agent is gone.
func (a *App) removeTaskTmp(id string) {
	if err := os.RemoveAll(a.taskTmp(id)); err != nil {
		a.Store.Event(id, "task_tmp_failed", "remove: "+err.Error())
	}
}

// taskTmpDead reports whether t's agent is gone for good, so its tmp dir
// can go: killed, landed, or a spawn that failed.
func taskTmpDead(t store.Task) bool {
	return !t.Active() || t.Status == StatusFailed
}

// taskTmps sorts the task tmp dirs into those of live tasks, which a sweep
// must keep, and orphans: dirs of dead or unknown tasks. A task list that
// can't be read makes every dir live, so nothing goes on a guess.
func (a *App) taskTmps() (live, orphans []string) {
	parent := a.taskTmpParent()
	es, err := os.ReadDir(parent)
	if err != nil {
		return nil, nil
	}
	ts, err := a.Store.Tasks()
	status := map[string]store.Task{}
	for _, t := range ts {
		status[t.ID] = t
	}
	for _, e := range es {
		id, ok := strings.CutPrefix(e.Name(), taskTmpPrefix)
		if !ok || !e.IsDir() {
			continue
		}
		p := filepath.Join(parent, e.Name())
		if t, known := status[id]; err != nil || (known && !taskTmpDead(t)) {
			live = append(live, p)
			continue
		}
		orphans = append(orphans, p)
	}
	return live, orphans
}

// taskTmpHolders are the paths a sweep keeps as held: what live processes
// hold, plus live tasks' tmp dirs, which an idle agent may not touch for
// hours.
func (a *App) taskTmpHolders() []string {
	live, _ := a.taskTmps()
	return append(scratch.ProcHolders(), live...)
}

// sweepTaskTmps removes the orphaned task tmp dirs the age sweep in res
// kept for being fresh or didn't see, and records them in res. One it kept
// as held by a live process stays. A dry run lists them and removes nothing.
func (a *App) sweepTaskTmps(res *scratch.Result, dry bool) {
	_, orphans := a.taskTmps()
	skip := map[string]bool{}
	for _, e := range res.Removed {
		skip[e.Path] = true
	}
	res.Kept = slices.DeleteFunc(res.Kept, func(e scratch.Entry) bool {
		if !slices.Contains(orphans, e.Path) {
			return false
		}
		skip[e.Path] = strings.HasPrefix(e.Reason, "held")
		return !skip[e.Path]
	})
	for _, p := range orphans {
		if skip[p] {
			continue
		}
		e := scratch.Entry{Path: p, Saddle: true}
		if fi, err := os.Lstat(p); err == nil {
			e.ModTime = fi.ModTime()
		}
		e.Bytes, e.Files = dirSize(p)
		if !dry {
			if err := os.RemoveAll(p); err != nil {
				e.Reason = "remove failed: " + err.Error()
				res.Kept = append(res.Kept, e)
				res.Errs = append(res.Errs, err)
				continue
			}
		}
		res.Removed = append(res.Removed, e)
		res.Freed += e.Bytes
	}
}

// dirSize is the bytes and entry count under p, not following symlinks.
func dirSize(p string) (int64, int) {
	var n int64
	files := 0
	_ = filepath.WalkDir(p, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		files++
		if fi, err := d.Info(); err == nil && !d.IsDir() {
			n += fi.Size()
		}
		return nil
	})
	return n, files
}

// shQuote quotes s for sh.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
