package app

import (
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
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
	from, skipped, err := gitx.LandedPrefix(t.Worktree, a.Cfg.Integration)
	if err != nil {
		return gitx.RebaseResult{}, err
	}
	rr, err := gitx.RebaseOnto(t.Worktree, a.Cfg.Integration, from, false)
	rr.Skipped = skipped
	a.Store.Event(task, "sync", fmt.Sprintf("ok=%v skipped=%d conflicts=%s", rr.OK, skipped, strings.Join(rr.Conflicts, ",")))
	return rr, err
}
