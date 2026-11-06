package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/checkpoint"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// StatusPaused marks a task saddle down (or SIGTERM on saddle up) stopped.
// It keeps its claims and worktree, and saddle up resumes it (#254).
const StatusPaused = "paused"

// StatusOrphaned is shown, never stored, for a live task whose window is
// gone and whose agent has been silent for OrphanAfter.
const StatusOrphaned = "orphaned"

// OrphanAfter is how long a windowless task may go without a heartbeat (an
// event from its hooks) before it is shown as orphaned.
var OrphanAfter = 10 * time.Minute

// EventResumeWatch is logged once the resume watcher's startup pass is done.
const EventResumeWatch = "resume_watch"

// ResumeEvery is how often saddle up looks for tasks whose window vanished.
var ResumeEvery = 15 * time.Second

// Auto-resume gives up on a task resumed resumeBurst times within
// resumeWindow: something kills it as fast as it starts. It shows as
// orphaned, and saddle resume or saddle rescue take it from there.
const (
	resumeBurst  = 3
	resumeWindow = 30 * time.Minute
)

const (
	resumedNote = "[saddle] saddle restarted and lost this window, so your session was resumed in a new one. " +
		"Carry on where you left off; check git status first."
	relaunchNote = "[saddle] Note: this task was restarted after its agent session was lost and could not be resumed. " +
		"Work may already exist in the worktree: check git log and git status, and continue from there rather than starting over."
)

// liveStatus is a status whose agent should have a window.
func liveStatus(s string) bool {
	switch s {
	case store.Running, store.Idle, store.NeedsYou, store.Conflict:
		return true
	}
	return false
}

// lost reports whether t is a worker that was launched and should be alive
// but has no window of its own. Paused tasks count only when paused is set.
func (a *App) lost(t store.Task, paused bool) bool {
	if t.Role != store.RoleWorker || t.Window == "" {
		return false // never launched (still spawning) or not ours to run
	}
	if !liveStatus(t.Status) && (!paused || t.Status != StatusPaused) {
		return false
	}
	return !a.ownWindow(t)
}

// Resumed is one task given a new window.
type Resumed struct {
	Task    string `json:"task"`
	Session string `json:"session,omitempty"` // the resumed session; empty when relaunched with the brief
	Window  string `json:"window"`
}

func (r Resumed) String() string {
	if r.Session != "" {
		return r.Task + " (session resumed)"
	}
	return r.Task + " (relaunched with its brief)"
}

// canResume reports whether t's agent session can be continued: a Claude
// Code task (not under the Grok harness) with a session whose transcript
// is on disk. Other adapters are relaunched with their brief.
func (a *App) canResume(t store.Task) bool {
	ad := a.taskAdapter(t)
	if ad.Name() != usage.Claude || a.Cfg.Harness == config.HarnessGrok || t.SessionID == "" {
		return false
	}
	p := ad.Usage().Transcript(t.Worktree, a.stateDir("run", t.ID), t.SessionID)
	_, err := os.Stat(p)
	return p != "" && err == nil
}

// resume opens a new window for t in its worktree, resuming its session
// when it can and relaunching with the original brief when it can't.
func (a *App) resume(t store.Task) (Resumed, error) {
	r := Resumed{Task: t.ID}
	fresh := false // a recreated worktree has no session to resume
	if fi, err := os.Stat(t.Worktree); err != nil || !fi.IsDir() {
		if err := a.recoverWorktree(t); err != nil {
			return r, err
		}
		fresh = true
	}
	all, err := a.Store.Claims()
	if err != nil {
		return r, err
	}
	prompt := strings.TrimSpace(t.Prompt + "\n\n" + relaunchNote)
	ad := a.taskAdapter(t)
	// An adapter or model out of quota would only park it again: relaunch
	// it on the next one with quota (#321, #483). Its session can't move
	// across adapters or models.
	if next, e, out, err := a.reroute(ad.Name(), t.Model, pickReq{claims: len(all[t.ID]) > 0, crowded: claimsCrowded(all[t.ID], all, t.ID)}, time.Now()); out && err == nil {
		if nad, err := agent.ByName(next.Adapter); err == nil {
			t.Model, t.Adapter = next.Model, next.Adapter
			if err := a.Store.SetField(t.ID, "model", t.Model); err != nil {
				return r, err
			}
			if err := a.Store.SetField(t.ID, "adapter", t.Adapter); err != nil {
				return r, err
			}
			if err := a.rotatedTask(t, e.Label(), nad, all[t.ID]); err != nil {
				return r, err
			}
			ad = nad
		}
	} else if !fresh && a.canResume(t) {
		r.Session, prompt = t.SessionID, resumedNote
	}
	win, err := a.start(t, all[t.ID], ad, r.Session, prompt)
	if err != nil {
		a.Store.Event(t.ID, "resume_failed", err.Error())
		return r, fmt.Errorf("%s: %w", t.ID, err)
	}
	r.Window = win
	if err := a.Store.SetStatus(t.ID, store.Running); err != nil {
		return r, err
	}
	how := "relaunched"
	if r.Session != "" {
		how = "session " + r.Session
	}
	a.Store.Event(t.ID, "resumed", how+" window "+win)
	return r, nil
}

// ResumeLost gives every live task whose window is gone (including when the
// whole tmux server or session is gone) a new one, and tells the
// orchestrator once, as a digest item. With paused it resumes paused tasks
// too, as saddle up does when it starts.
func (a *App) ResumeLost(paused bool) ([]Resumed, error) {
	return a.resumeLost(paused, nil)
}

func (a *App) resumeLost(paused bool, allow func(store.Task) bool) ([]Resumed, error) {
	ts, err := a.Store.Tasks()
	if err != nil {
		return nil, err
	}
	var out []Resumed
	var errs []error
	for _, t := range ts {
		if !a.lost(t, paused) || (allow != nil && !allow(t)) {
			continue
		}
		// Read it again: a kill or done may have landed since the list.
		if t, err = a.Store.Task(t.ID); err != nil || !a.lost(t, paused) {
			continue
		}
		r, err := a.resume(t)
		if errors.Is(err, errTaskRetired) {
			continue // retired, told once: nothing left to resume (#765)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		out = append(out, r)
	}
	if len(out) > 0 {
		names := make([]string, len(out))
		for i, r := range out {
			names[i] = r.String()
		}
		noun := "tasks"
		if len(out) == 1 {
			noun = "task"
		}
		errs = append(errs, a.Notify(OrchestratorID, store.NoticeInfo,
			fmt.Sprintf("Resumed %d %s whose windows were gone: %s.", len(out), noun, strings.Join(names, ", "))))
	}
	return out, errors.Join(errs...)
}

// Resume gives one task a new window: saddle resume <task>, for an orphaned
// or paused task. It refuses a task whose window is alive.
func (a *App) Resume(task string) (Resumed, error) {
	t, err := a.Store.Task(task)
	if err != nil {
		return Resumed{Task: task}, err
	}
	if t.Role != store.RoleWorker {
		return Resumed{Task: task}, fmt.Errorf("%s is not a worker task", task)
	}
	if !liveStatus(t.Status) && t.Status != StatusPaused {
		return Resumed{Task: task}, fmt.Errorf("%s is %s; only running, idle, needs-you, conflict or paused tasks resume", task, t.Status)
	}
	if t.Window != "" && a.ownWindow(t) {
		return Resumed{Task: task}, fmt.Errorf("%s already has a live window %s", task, t.Window)
	}
	return a.resume(t)
}

// RunResumeWatcher runs as long as saddle up (or the plugin engine): at once
// it resumes every lost or paused task, then every ResumeEvery it resumes
// tasks whose window vanished. Failures are events.
func (a *App) RunResumeWatcher(ctx context.Context) {
	recent := map[string][]time.Time{}
	allow := func(t store.Task) bool {
		var keep []time.Time
		for _, at := range recent[t.ID] {
			if time.Since(at) < resumeWindow {
				keep = append(keep, at)
			}
		}
		recent[t.ID] = keep
		if len(keep) >= resumeBurst {
			if len(keep) == resumeBurst {
				a.Store.Event(t.ID, "resume_gave_up", fmt.Sprintf("resumed %d times in %s", resumeBurst, resumeWindow))
				recent[t.ID] = append(keep, time.Now()) // log it once
			}
			return false
		}
		recent[t.ID] = append(keep, time.Now())
		return true
	}
	tick := func(paused bool) {
		if _, err := a.resumeLost(paused, allow); err != nil {
			a.Store.Event("", "resume_error", err.Error())
		}
	}
	tick(true)
	a.Store.Event("", EventResumeWatch, "every "+ResumeEvery.String())
	tk := time.NewTicker(ResumeEvery)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
			tick(false)
		}
	}
}

// Orphans returns the live tasks that lost their window and have had no
// heartbeat for OrphanAfter.
func (a *App) Orphans() (map[string]bool, error) {
	ts, err := a.Store.Tasks()
	if err != nil {
		return nil, err
	}
	beats, err := a.Store.LastEventTimes()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, t := range ts {
		last := beats[t.ID]
		if last.Before(t.CreatedAt) {
			last = t.CreatedAt
		}
		// The heartbeat first: it is a map lookup, the window a tmux call.
		if time.Since(last) > OrphanAfter && a.lost(t, false) {
			out[t.ID] = true
		}
	}
	return out, nil
}

// OrphanHint says what to do about an orphaned task.
func OrphanHint(task string) string {
	return fmt.Sprintf("no window and no heartbeat for %s; saddle resume %s or saddle rescue %s", OrphanAfter, task, task)
}

// Pause stops every live worker for saddle down or SIGTERM: it snapshots
// uncommitted work, marks the task paused (claims and worktree kept) and
// stops the tmux session. It returns the paused tasks.
func (a *App) Pause() ([]string, error) {
	ts, err := a.Store.Tasks()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range ts {
		if t.Role != store.RoleWorker || t.Window == "" || !liveStatus(t.Status) {
			continue
		}
		a.snapshotWIP(t.ID, t.Worktree)
		if err := a.Store.SetStatus(t.ID, StatusPaused); err != nil {
			return out, err
		}
		a.Store.Event(t.ID, "paused", "")
		out = append(out, t.ID)
	}
	if a.Tmux.HasSession() {
		if err := a.Tmux.KillSession(); err != nil {
			return out, err
		}
	}
	return out, nil
}

// snapshotCommit commits everything in dir's worktree, untracked files
// included, on top of HEAD without touching the worktree, its index or its
// branch. It returns "" when there is nothing uncommitted.
func snapshotCommit(dir, msg string) (string, error) { return checkpoint.Commit(dir, msg) }

// wipRef is where a task's uncommitted work is snapshotted before kill, gc
// and down (#50).
func wipRef(task string) string { return "refs/saddle/wip/" + task }

// snapshotWIP saves dir's uncommitted work to refs/saddle/wip/<task>. It is
// best effort: a failure is an event, and never blocks the kill.
func (a *App) snapshotWIP(task, dir string) {
	c, err := snapshotCommit(dir, "saddle: uncommitted work of "+task)
	if err == nil && c != "" {
		_, err = gitx.Run(a.Root, "update-ref", wipRef(task), c)
	}
	switch {
	case err != nil:
		a.Store.Event(task, "wip_snapshot_failed", err.Error())
	case c != "":
		a.Store.Event(task, "wip_snapshot", wipRef(task)+" "+c)
	}
}

// Rescued is what saddle rescue saved.
type Rescued struct {
	Task   string `json:"task"`
	Branch string `json:"branch"`         // rescue/<task>: the branch plus uncommitted work
	Diff   string `json:"diff,omitempty"` // the uncommitted work as a patch; empty when there was none
}

// Rescue saves an orphaned (or any live) task's work and kills it: its
// uncommitted changes are committed on top of its branch as rescue/<task>
// and written as a patch under .saddle/rescue/, then the task is killed
// with its worktree kept and its claims released.
func (a *App) Rescue(task string) (Rescued, error) {
	r := Rescued{Task: task}
	t, err := a.Store.Task(task)
	if err != nil {
		return r, err
	}
	if t.Role != store.RoleWorker || !t.Active() {
		return r, fmt.Errorf("%s is %s; nothing to rescue", task, t.Status)
	}
	tip := ""
	if fi, err := os.Stat(t.Worktree); err == nil && fi.IsDir() {
		if tip, err = gitx.Run(t.Worktree, "rev-parse", "HEAD"); err != nil {
			return r, err
		}
	} else if tip, err = gitx.RevParse(a.Root, t.Branch); err != nil {
		return r, fmt.Errorf("%s has neither a worktree nor a branch to rescue", task)
	}
	snap, err := snapshotCommit(t.Worktree, "saddle rescue: uncommitted work of "+task)
	if err != nil {
		return r, err
	}
	if snap != "" {
		diff, err := gitx.Run(t.Worktree, "diff", "--binary", tip, snap)
		if err != nil {
			return r, err
		}
		r.Diff = a.stateDir("rescue", task+".diff")
		if err := os.MkdirAll(filepath.Dir(r.Diff), 0o755); err != nil {
			return r, err
		}
		if err := os.WriteFile(r.Diff, []byte(diff+"\n"), 0o644); err != nil {
			return r, err
		}
		tip = snap
	}
	r.Branch = "rescue/" + task
	if gitx.BranchExists(a.Root, r.Branch) {
		r.Branch = fmt.Sprintf("rescue/%s-%d", task, time.Now().Unix())
	}
	if _, err := gitx.Run(a.Root, "branch", r.Branch, tip); err != nil {
		return r, err
	}
	// Dead first, so the resume watcher never brings it back.
	note := "killed with snapshot " + r.Branch
	a.recordKiller(task, KilledByRescue) // its work is saved: not a decline (#772)
	if err := errors.Join(a.Store.Release(task), a.Store.SetStatus(task, store.Killed), a.Store.SetField(task, "summary", note)); err != nil {
		return r, err
	}
	if t.Window != "" && a.ownWindow(t) {
		_ = a.Tmux.KillWindow(t.Window)
	}
	a.Store.Event(task, "rescue", strings.TrimSpace(r.Branch+" "+r.Diff))
	return r, nil
}

// A task whose worktree vanished (#765) is rebuilt from its branch, or
// retired when there is nothing to rebuild it from.
const (
	eventWorktreeRecreated = "worktree_recreated" // data: the branch it was recreated at
	eventWorktreeLost      = "worktree_lost"      // data: why the task was retired
	KilledByWorktreeLost   = "worktree_lost"      // the killer of a retired task
)

// errTaskRetired is resume's answer for a task it retired instead.
var errTaskRetired = errors.New("retired")

// recoverWorktree rebuilds t's missing worktree at its branch, local or on
// origin, when the branch has work beyond integration. Otherwise it retires
// t and returns errTaskRetired. Either way it logs one event and tells the
// orchestrator once, so the resume watcher never errors on it every tick.
func (a *App) recoverWorktree(t store.Task) error {
	_, _ = gitx.Run(a.Root, "worktree", "prune") // forget the vanished one
	integ := a.Cfg.Integration
	var why string
	switch remote := "refs/remotes/origin/" + t.Branch; {
	case t.Branch == "":
		why = "it has no branch"
	case gitx.BranchExists(a.Root, t.Branch):
		if a.unique(t.Branch) == 0 {
			why = fmt.Sprintf("its branch %s has no commits beyond %s", t.Branch, integ)
		} else if _, err := gitx.Run(a.Root, "worktree", "add", t.Worktree, t.Branch); err != nil {
			why = fmt.Sprintf("recreating it at %s failed: %s", t.Branch, oneLine(err.Error()))
		}
	default:
		if _, err := gitx.RevParse(a.Root, remote); err != nil {
			why = "its branch " + t.Branch + " is gone too"
		} else if n, err := gitx.CommitsBetween(a.Root, integ, remote); err == nil && n == 0 {
			why = fmt.Sprintf("its branch %s has no commits beyond %s", t.Branch, integ)
		} else if err := gitx.WorktreeAdd(a.Root, t.Worktree, t.Branch, remote); err != nil {
			why = fmt.Sprintf("recreating it from %s failed: %s", remote, oneLine(err.Error()))
		}
	}
	if why != "" {
		return a.retireLost(t, why)
	}
	a.Store.Event(t.ID, eventWorktreeRecreated, t.Branch)
	return a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
		"%s %q lost its worktree, so saddle recreated it at %s and relaunched the task with its brief. Nothing needs doing.",
		t.ID, t.Title, t.Branch))
}

// retireLost kills t, whose worktree is gone with nothing to rebuild it
// from: its claims are released and its processes reaped, and a CI repair's
// red layer is freed for a fresh repair. Saddle killed it, not the owner.
func (a *App) retireLost(t store.Task, why string) error {
	a.recordKiller(t.ID, KilledByWorktreeLost)
	if err := a.Kill(t.ID, false); err != nil {
		return fmt.Errorf("%s: worktree %s is gone and retiring it failed: %w", t.ID, t.Worktree, err)
	}
	a.Store.Event(t.ID, eventWorktreeLost, why)
	if _, err := a.ReapCIRepairs(); err != nil {
		a.Store.Event(t.ID, "ci_repair_reap_failed", err.Error())
	}
	if err := a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
		"saddle retired %s %q: its worktree %s is gone and %s, so there was nothing to resume. Its claims are released.",
		t.ID, t.Title, t.Worktree, why)); err != nil {
		return err
	}
	return fmt.Errorf("%s: worktree is gone and %s, so saddle %w it", t.ID, why, errTaskRetired)
}
