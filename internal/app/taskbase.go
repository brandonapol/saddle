package app

import (
	"fmt"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// A task's base (ADR 0012 rule 1, #932) is the branch its branch is cut from
// and its PR targets: the repo's base (main), or the branch of the one
// unmerged task it declared `after`. Nothing else stacks a task: not a spawn
// parent, an issue or the task's text. The base is stored on the task
// (tasks.base); sync and done rebase onto it.

// spawnBase picks the base of a task spawned after deps, and the ref its
// branch starts at. Two unmerged deps are refused: one branch can be cut from
// only one of them.
func (a *App) spawnBase(id string, after []string) (base, start string, err error) {
	main := a.freshBase()
	var open []store.Task
	for _, d := range after {
		dep, err := a.Store.Task(d)
		if err != nil {
			return "", "", fmt.Errorf("spawn: after: no task %s", d)
		}
		if !a.unmergedDep(dep, main) {
			continue
		}
		open = append(open, dep)
	}
	switch len(open) {
	case 0:
		return a.Cfg.Base, main, nil
	case 1:
		return open[0].Branch, open[0].Branch, nil
	}
	var ids []string
	for _, d := range open {
		ids = append(ids, d.ID)
	}
	return "", "", fmt.Errorf("spawn: %s would run after %s, which are all unmerged, but a task's branch can be cut from only one unmerged branch (ADR 0012). "+
		"Split the task so each part runs after one of them, or wait until all but one has merged into %s and spawn it again",
		id, strings.Join(ids, " and "), a.Cfg.Base)
}

// unmergedDep reports whether dep's branch is unmerged work a task running
// after it must be cut from. A killed, failed or no-change task has nothing
// to build on, and a branch whose tip main already holds is merged.
func (a *App) unmergedDep(dep store.Task, main string) bool {
	switch dep.Status {
	case store.Killed, store.Failed, store.NoChange:
		return false
	}
	if dep.Branch == "" || !gitx.BranchExists(a.Root, dep.Branch) {
		return false
	}
	return !a.depMerged(dep, main)
}

// depMerged reports whether main holds dep's branch tip. A live task's
// branch with no commits of its own is always "held" by main, so for a task
// still working it is not merged yet: its work is to come.
func (a *App) depMerged(dep store.Task, main string) bool {
	switch dep.Status {
	case store.Running, store.Idle, store.NeedsYou, store.Conflict, store.Done:
		return false
	}
	_, err := gitx.Run(a.Root, "merge-base", "--is-ancestor", dep.Branch, main)
	return err == nil
}

// taskBase is t's base branch: the stored one, or the repo's base for tasks
// from before it was stored.
func (a *App) taskBase(t store.Task) string {
	if t.Base != "" {
		return t.Base
	}
	return a.Cfg.Base
}

// rebaseTarget is where sync and done rebase a task's branch.
type rebaseTarget struct {
	Onto   string // the ref to rebase onto: <remote>/<base>, the local base, or the after task's branch
	PRBase string // the branch its PR targets
	// From, when set, replays only From..HEAD (`rebase --onto Onto From`):
	// the after task merged, so its commits below this branch are dropped.
	From string
}

// resolveBase is t's rebase target. With fetch, the repo's base is fetched
// first. An after task whose branch has since merged into the repo's base
// moves t's base to the repo's base, and that is stored.
func (a *App) resolveBase(t store.Task, fetch bool) rebaseTarget {
	main := a.Cfg.Base
	if fetch {
		main = a.freshBase()
	} else if r := a.remote(); r != "" {
		if _, err := gitx.RevParse(a.Root, r+"/"+a.Cfg.Base); err == nil {
			main = r + "/" + a.Cfg.Base
		}
	}
	base := a.taskBase(t)
	if base == a.Cfg.Base {
		return rebaseTarget{Onto: main, PRBase: a.Cfg.Base}
	}
	merged := false
	if id, ok := a.taskOnBranch(base); ok {
		if dep, err := a.Store.Task(id); err == nil {
			merged = a.depMerged(dep, main)
		}
	}
	if merged || !gitx.BranchExists(a.Root, base) {
		// Its after task merged (or its branch is gone): back onto main.
		from := ""
		if gitx.BranchExists(a.Root, base) {
			from = base
		}
		if err := a.Store.SetField(t.ID, "base", a.Cfg.Base); err == nil {
			a.Store.Event(t.ID, EventBaseMoved, base+" → "+a.Cfg.Base)
		}
		return rebaseTarget{Onto: main, PRBase: a.Cfg.Base, From: from}
	}
	return rebaseTarget{Onto: base, PRBase: base}
}

// EventAdopts is a task spawned on another task's branch (a finisher): the
// other task's commits on it count as its own at done. Data: that task's id.
const EventAdopts = "adopts"

// EventBaseMoved is a task's base moving to the repo's base because the task
// it ran after merged. Data: "old → new".
const EventBaseMoved = "base_moved"
