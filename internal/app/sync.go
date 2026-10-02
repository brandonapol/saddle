package app

import (
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// Sync rebases a task's branch onto the integration branch. Commits whose
// work integration already holds under other SHAs (squash-landed) are not
// replayed. Conflicts are left in place for the agent to resolve; any other
// failure is aborted, and a rebase already in progress is reported rather
// than started over.
func (a *App) Sync(task string) (gitx.RebaseResult, error) {
	t, err := a.Store.Task(task)
	if err != nil {
		return gitx.RebaseResult{}, err
	}
	if gitx.RebaseInProgress(t.Worktree) {
		return gitx.RebaseResult{}, fmt.Errorf("a rebase is already in progress in %s: resolve the conflicts, `git add` them and `git rebase --continue`, or run `git rebase --abort` to start over", t.Worktree)
	}
	if dirty, _ := gitx.Dirty(t.Worktree); len(dirty) > 0 {
		return gitx.RebaseResult{}, fmt.Errorf("commit your changes before syncing:\n%s", strings.Join(dirty, "\n"))
	}
	from, skipped, err := a.replayFrom(t, a.Cfg.Integration)
	if err != nil {
		return gitx.RebaseResult{}, err
	}
	rr, err := gitx.RebaseOnto(t.Worktree, a.Cfg.Integration, from, false)
	rr.Skipped = skipped
	a.Store.Event(task, "sync", fmt.Sprintf("ok=%v skipped=%d conflicts=%s", rr.OK, skipped, strings.Join(rr.Conflicts, ",")))
	return rr, err
}

// replayFrom is where rebasing t's branch onto onto (integration or its
// head) starts replaying: past the commits integration already holds under
// other SHAs, squash-landed or rewritten by restack, so only t's own work is
// replayed (#147). "" means a plain rebase. A task that has landed before
// doesn't use integration's fork point, which may be its own landed head.
func (a *App) replayFrom(t store.Task, onto string) (string, int, error) {
	track := a.Cfg.Integration
	if a.everLanded(t.ID) {
		track = ""
	}
	return gitx.ReplayFrom(t.Worktree, onto, track)
}

// everLanded reports whether the train ever landed task.
func (a *App) everLanded(task string) bool {
	es, err := a.Store.Events(-1)
	if err != nil {
		return true // can't tell: don't trust the fork point
	}
	for _, e := range es {
		if e.Task == task && e.Kind == "landed" {
			return true
		}
	}
	return false
}

// autoRebased is what autoRebase did to a live task's branch.
type autoRebased struct {
	From, To  string   // HEAD before and after, when it moved
	Conflicts []string // files a rebase would conflict in; nothing moved
	Skipped   string   // why it wasn't tried, or ""
}

// autoRebase rebases a live task's branch onto integration as the train,
// right after a landing, the way Sync would (#26). It only touches a clean
// worktree with the task's branch checked out; on conflict it aborts and
// leaves the branch as it was, so the agent resolves it with saddle sync.
func (a *App) autoRebase(t store.Task) autoRebased {
	switch {
	case a.Cfg.Train.NoAutoRebase:
		return autoRebased{Skipped: "auto-rebase is off"}
	case !a.checkedOut(t):
		return autoRebased{Skipped: "its worktree is gone or has another branch checked out"}
	case gitx.RebaseInProgress(t.Worktree):
		return autoRebased{Skipped: "a rebase is already in progress"}
	}
	if dirty, _ := gitx.Dirty(t.Worktree); len(dirty) > 0 {
		return autoRebased{Skipped: "it has uncommitted changes"}
	}
	head, err := gitx.RevParse(t.Worktree, "HEAD")
	if err != nil {
		return autoRebased{Skipped: err.Error()}
	}
	integ, err := gitx.RevParse(a.Root, a.Cfg.Integration)
	if err != nil {
		return autoRebased{Skipped: err.Error()}
	}
	if mb, _ := gitx.Run(t.Worktree, "merge-base", "HEAD", integ); mb == integ {
		return autoRebased{Skipped: "it is already on " + a.Cfg.Integration}
	}
	from, _, err := a.replayFrom(t, integ)
	if err != nil {
		return autoRebased{Skipped: err.Error()}
	}
	args := []string{"-c", "merge.directoryRenames=true", "-c", "merge.renames=true",
		"-c", "rerere.enabled=true", "-c", "core.editor=true", "rebase"}
	if from != "" {
		args = append(args, "--onto", integ, from)
	} else {
		args = append(args, integ)
	}
	if _, err := trainGit(t.Worktree, args...); err != nil {
		conf, _ := gitx.Run(t.Worktree, "diff", "--name-only", "--diff-filter=U")
		if gitx.RebaseInProgress(t.Worktree) {
			_, _ = trainGit(t.Worktree, "rebase", "--abort")
		}
		if conf == "" {
			return autoRebased{Skipped: "the rebase failed: " + err.Error()}
		}
		a.Store.Event(t.ID, "auto_rebase", "conflicts="+strings.ReplaceAll(conf, "\n", ","))
		return autoRebased{Conflicts: strings.Split(conf, "\n")}
	}
	to, _ := gitx.RevParse(t.Worktree, "HEAD")
	a.Store.Event(t.ID, "auto_rebase", short(head)+" → "+short(to))
	return autoRebased{From: head, To: to}
}
