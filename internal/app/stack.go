package app

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// RestackMove is one ref restack moved.
type RestackMove struct {
	Task string `json:"task,omitempty"`
	Ref  string `json:"ref"`
	Old  string `json:"old"`
	New  string `json:"new"`
}

// RestackResult reports what a restack did.
type RestackResult struct {
	Base       string        `json:"base"` // the origin/<base> commit the stack now sits on
	Moves      []RestackMove `json:"moves,omitempty"`
	Merged     []string      `json:"merged,omitempty"`  // tasks whose work base already has
	Dropped    int           `json:"dropped,omitempty"` // landed commits base already has
	Retargeted []string      `json:"retargeted,omitempty"`
}

// restacked is a landed task's place in the rebuilt stack: NewFrom..NewTo.
type restacked struct {
	landedTask
	NewFrom, NewTo string
}

func (r restacked) gone() bool { return r.NewFrom == r.NewTo }

// RestackConflict is a landed commit that no longer applies on the new base.
type RestackConflict struct {
	Task   string
	Commit string
	Files  []string
}

func (c *RestackConflict) Error() string {
	return fmt.Sprintf("restack stopped: %s's commit %s conflicts in %s; nothing was moved and %s was told",
		c.Task, short(c.Commit), strings.Join(c.Files, ", "), c.Task)
}

// Restack rebuilds the landed stack on origin/<base> after the base moved or a
// bottom PR merged. Under the train lock it replays each task's own landed
// commits in train order, dropping those base already has (same patch-id, or
// the task's files already match, as after a squash merge). Only once every
// task replays cleanly does it move refs, each one compare-and-swap: the task
// branches, then integration. It then force-with-lease pushes the moved
// branches and retargets the PRs; a merged task's PR is skipped and the next
// one targets base. A conflict stops it before any ref moves and goes back to
// the task that owns the commit. Restack never resolves one itself.
func (a *App) Restack() (RestackResult, error) {
	var res RestackResult
	unlock, err := a.lockTrain()
	if err != nil {
		return res, err
	}
	defer unlock()
	if br, _ := gitx.CurrentBranch(a.Root); br == a.Cfg.Integration {
		return res, fmt.Errorf("%s is checked out in %s; switch it to another branch so restack can move it", a.Cfg.Integration, a.Root)
	}
	remote := "refs/remotes/origin/" + a.Cfg.Base
	if _, err := trainGit(a.Root, "fetch", "origin", "+refs/heads/"+a.Cfg.Base+":"+remote); err != nil {
		return res, err
	}
	if res.Base, err = gitx.RevParse(a.Root, remote); err != nil {
		return res, err
	}
	stack, err := a.landedStack()
	if err != nil {
		return res, err
	}
	if len(stack) == 0 {
		return res, errors.New("nothing has landed yet")
	}
	if err := a.checkDrift(stack); err != nil {
		return res, err
	}
	integ, err := gitx.RevParse(a.Root, a.Cfg.Integration)
	if err != nil {
		return res, err
	}
	mb, err := gitx.Run(a.Root, "merge-base", res.Base, integ)
	if err != nil {
		return res, err
	}
	ids, err := patchIDs(a.Root, mb+".."+res.Base)
	if err != nil {
		return res, err
	}
	inBase := map[string]bool{}
	for _, id := range ids {
		inBase[id] = true
	}
	if err := a.checkIntegration(stack[len(stack)-1], integ, inBase); err != nil {
		return res, err
	}

	plan, dropped, err := a.replay(stack, res.Base, inBase)
	var conflict *RestackConflict
	if errors.As(err, &conflict) {
		a.returnConflict(conflict)
	}
	if err != nil {
		return res, err
	}
	res.Dropped = dropped

	if err := a.moveStack(plan, integ, &res); err != nil {
		return res, err
	}
	if err := a.republish(plan, &res); err != nil {
		return res, err
	}
	msg := fmt.Sprintf("Restacked %d landed tasks onto origin/%s at %s: %d refs moved, %d commits already in base dropped.",
		len(plan), a.Cfg.Base, short(res.Base), len(res.Moves), res.Dropped)
	if len(res.Merged) > 0 {
		msg += " Merged, so skipped: " + strings.Join(res.Merged, ", ") + "."
	}
	if err := a.Notify(OrchestratorID, store.NoticeInfo, msg); err != nil {
		return res, err
	}
	return res, nil
}

// checkIntegration refuses when integration holds work restack would lose:
// it must contain the last landed commit, and anything after that must
// already be in base (e.g. a merge of base into integration).
func (a *App) checkIntegration(last landedTask, integ string, inBase map[string]bool) error {
	if _, err := gitx.Run(a.Root, "merge-base", "--is-ancestor", last.To, integ); err != nil {
		return fmt.Errorf("%s doesn't contain %s's landed commit %s, so restack can't tell what is on it; nothing was moved",
			a.Cfg.Integration, last.ID, short(last.To))
	}
	extra, err := patchIDs(a.Root, last.To+".."+integ)
	if err != nil {
		return err
	}
	n := 0
	for _, id := range extra {
		if !inBase[id] {
			n++
		}
	}
	if n > 0 {
		return fmt.Errorf("%s has %s after %s's landed commit that no landed task owns and base doesn't have; restack would drop them, so nothing was moved",
			a.Cfg.Integration, plural(n, "commit"), last.ID)
	}
	return nil
}

// replay rebuilds the stack on base in a scratch worktree and returns each
// task's new range. No ref outside the scratch worktree moves.
func (a *App) replay(stack []landedTask, base string, inBase map[string]bool) ([]restacked, int, error) {
	dir := a.stateDir("restack")
	_ = os.RemoveAll(dir)
	_, _ = gitx.Run(a.Root, "worktree", "prune")
	if _, err := gitx.Run(a.Root, "worktree", "add", "--detach", dir, base); err != nil {
		return nil, 0, err
	}
	defer func() { _ = gitx.WorktreeRemove(a.Root, dir) }()

	var plan []restacked
	dropped := 0
	tip, prev := base, ""
	for _, l := range stack {
		r := restacked{landedTask: l, NewFrom: tip, NewTo: tip}
		from := l.From
		if from == "" {
			// Recorded before the train kept ranges: the task starts where the one before it ended.
			if from = prev; from == "" {
				from, _ = gitx.Run(a.Root, "merge-base", a.Cfg.Base, l.To)
			}
		}
		prev = l.To
		if l.merged() || a.alreadyApplied(from, l.To, tip) {
			plan = append(plan, r)
			continue
		}
		commits, err := gitx.Run(a.Root, "rev-list", "--reverse", "--no-merges", from+".."+l.To)
		if err != nil {
			return nil, 0, err
		}
		for _, c := range strings.Fields(commits) {
			if ids, _ := patchIDs(a.Root, c+"^!"); len(ids) == 1 && inBase[ids[0]] {
				dropped++
				continue
			}
			empty, err := a.pick(dir, l.ID, c)
			if err != nil {
				return nil, 0, err
			}
			if empty {
				dropped++
			}
		}
		if tip, err = gitx.RevParse(dir, "HEAD"); err != nil {
			return nil, 0, err
		}
		r.NewTo = tip
		plan = append(plan, r)
	}
	return plan, dropped, nil
}

// alreadyApplied reports whether every file the task changed already has the
// task's content at tip. That is how a squash merge shows up: one new commit
// on base whose patch-id matches none of the task's.
func (a *App) alreadyApplied(from, to, tip string) bool {
	files, err := gitx.ChangedFiles(a.Root, from, to)
	if err != nil || len(files) == 0 {
		return false
	}
	_, err = gitx.Run(a.Root, append([]string{"diff", "--quiet", to, tip, "--"}, files...)...)
	return err == nil
}

// pick cherry-picks c in dir. It reports a commit that became empty (base
// already has its change) and returns a *RestackConflict on conflict.
func (a *App) pick(dir, task, c string) (empty bool, err error) {
	_, err = trainGit(dir, "-c", "merge.directoryRenames=true", "-c", "merge.renames=true",
		"-c", "core.editor=true", "cherry-pick", "--allow-empty-message", c)
	if err == nil {
		return false, nil
	}
	conf, _ := gitx.Run(dir, "diff", "--name-only", "--diff-filter=U")
	if conf == "" {
		if _, e := gitx.RevParse(dir, "CHERRY_PICK_HEAD"); e == nil {
			_, err := trainGit(dir, "cherry-pick", "--skip")
			return true, err
		}
		return false, err
	}
	_, _ = trainGit(dir, "cherry-pick", "--abort")
	return false, &RestackConflict{Task: task, Commit: c, Files: strings.Split(conf, "\n")}
}

// returnConflict hands a restack conflict to the task that owns the commit and
// tells the orchestrator. Saddle never resolves it.
func (a *App) returnConflict(c *RestackConflict) {
	files := strings.Join(c.Files, ", ")
	subject, _ := gitx.Run(a.Root, "log", "-1", "--format=%s", c.Commit)
	a.Store.Event(c.Task, "restack_conflict", fmt.Sprintf("%s %s", c.Commit, files))
	_ = a.Notify(c.Task, store.NoticeAction, fmt.Sprintf(
		"Restack stopped at your landed commit %s %q: it conflicts with origin/%s in %s. Nothing was moved.\n"+
			"This conflict is yours to resolve; saddle won't pick a side. Rebase your work onto origin/%s, keep both sides' intent, run the tests, and tell the orchestrator when it's ready.",
		short(c.Commit), subject, a.Cfg.Base, files, a.Cfg.Base))
	_ = a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
		"Restack stopped: %s's landed commit %s conflicts with origin/%s in %s. Nothing was moved; %s has the conflict.",
		c.Task, short(c.Commit), a.Cfg.Base, files, c.Task))
}

// moveStack moves every task branch to its rebuilt commit, then integration
// to the new tip. Each move is compare-and-swap and recorded in events.
func (a *App) moveStack(plan []restacked, integ string, res *RestackResult) error {
	// Refuse before moving anything if a branch to move is checked out dirty.
	for _, r := range plan {
		if !r.gone() && r.NewTo != r.To && a.checkedOut(r.Task) {
			if dirty, _ := gitx.Dirty(r.Worktree); len(dirty) > 0 {
				return fmt.Errorf("%s's worktree %s has uncommitted changes; nothing was moved", r.ID, r.Worktree)
			}
		}
	}
	move := func(task, ref, old, new string) error {
		if _, err := trainGit(a.Root, "update-ref", ref, new, old); err != nil {
			return fmt.Errorf("%s moved during restack: %w", ref, err)
		}
		res.Moves = append(res.Moves, RestackMove{Task: task, Ref: ref, Old: old, New: new})
		a.Store.Event(task, "restack", fmt.Sprintf("%s %s → %s", ref, short(old), short(new)))
		return nil
	}
	for _, r := range plan {
		switch {
		case r.gone():
			res.Merged = append(res.Merged, r.ID)
			if !r.merged() {
				a.Store.Event(r.ID, "restack_merged", "base already has "+r.ID+"'s work")
			}
		case r.NewTo != r.To:
			if err := move(r.ID, "refs/heads/"+r.Branch, r.To, r.NewTo); err != nil {
				return err
			}
			if a.checkedOut(r.Task) {
				if _, err := trainGit(r.Worktree, "reset", "-q", "--hard"); err != nil {
					return err
				}
			}
		}
		if err := a.Store.SetTrain(r.ID, store.TrainOK, r.NewFrom+".."+r.NewTo, false); err != nil {
			return err
		}
	}
	if tip := plan[len(plan)-1].NewTo; tip != integ {
		return move("", "refs/heads/"+a.Cfg.Integration, integ, tip)
	}
	return nil
}

// checkedOut reports whether t's worktree still exists with its branch checked out.
func (a *App) checkedOut(t store.Task) bool {
	if t.Worktree == "" {
		return false
	}
	if _, err := os.Stat(t.Worktree); err != nil {
		return false
	}
	br, err := gitx.CurrentBranch(t.Worktree)
	return err == nil && br == t.Branch
}

// republish force-with-lease pushes the moved branches that have PRs and
// retargets those PRs: each targets the nearest unmerged branch below it, or
// base. A merged task's PR is left alone.
func (a *App) republish(plan []restacked, res *RestackResult) error {
	base := a.Cfg.Base
	for _, r := range plan {
		if r.gone() {
			continue
		}
		if r.PR != "" {
			if r.NewTo != r.To {
				if _, err := trainGit(a.Root, "push", "--force-with-lease=refs/heads/"+r.Branch, "origin", r.NewTo+":refs/heads/"+r.Branch); err != nil {
					return err
				}
				a.Store.Event(r.ID, "restack_push", r.Branch+" "+short(r.NewTo))
			}
			if _, err := gh(a.Root, "pr", "edit", r.PR, "--base", base); err != nil {
				return err
			}
			res.Retargeted = append(res.Retargeted, r.ID)
			a.Store.Event(r.ID, "restack_retarget", r.PR+" → "+base)
		}
		base = r.Branch
	}
	return nil
}
