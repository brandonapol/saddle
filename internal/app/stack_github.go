package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// People use GitHub too (#119): they merge PRs in the UI, squash-merge, merge
// a stacked PR into the branch below it, close PRs and delete branches, and
// the orchestrator kills tasks that already landed. The stack follows them
// here instead of freezing until someone edits state.db.

// Train states for landed tasks that have left the PR stack for good. Their
// note keeps the range they landed. TrainSuperseded is the literal the owner
// first set by hand, so rows like that keep working.
const (
	TrainMerged     = "merged"     // base has the task's work: its PR merged, or restack found it there
	TrainSuperseded = "superseded" // killed, PR closed, or unstacked: its work no longer ships on its own
)

// TrainHeld is the land result for a queued task the at-risk flag holds back
// because it touches the broken layers. Its train entry stays queued.
const TrainHeld = "held"

// PRInfo is what GitHub says about a pull request.
type PRInfo struct {
	State       string `json:"state"`                 // OPEN, CLOSED or MERGED
	Mergeable   string `json:"mergeable"`             // MERGEABLE, CONFLICTING or UNKNOWN
	BaseRefName string `json:"baseRefName,omitempty"` // the branch it targets, or was merged into
}

// PRLookup asks GitHub about the PR at url.
type PRLookup func(url string) (PRInfo, error)

// prView asks GitHub about a PR through gh, run in dir. Tests replace it.
var prView = func(dir, url string) (PRInfo, error) {
	var pr PRInfo
	out, err := gh(dir, "pr", "view", url, "--json", "state,mergeable,baseRefName")
	if err != nil {
		return pr, err
	}
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return pr, fmt.Errorf("gh pr view %s: %w", url, err)
	}
	return pr, nil
}

func (a *App) ghLookup(url string) (PRInfo, error) { return prView(a.Root, url) }

// ReconcileStack takes the landed tasks that are done with out of the PR
// stack, for good: killed tasks, closed PRs, and PRs merged into base (by
// merge, squash or rebase; GitHub's word is enough). A PR merged into another
// branch of the stack instead of base, whose work base doesn't have, is
// detached from its task so the next prs re-lands it as a fresh PR; the
// orchestrator hears about it once. A PR GitHub can't be asked about stays.
// It returns what GitHub said about each PR still in the stack.
func (a *App) ReconcileStack(lookup PRLookup) (map[string]PRInfo, error) {
	all, err := a.landedAll()
	if err != nil {
		return nil, err
	}
	info := map[string]PRInfo{}
	fetched := false
	var errs []error
	for _, l := range all {
		if l.State != store.TrainOK {
			continue
		}
		if l.Status == store.Killed {
			errs = append(errs, a.leaveStack(l, TrainSuperseded, "the task was killed"))
			continue
		}
		if l.PR == "" {
			continue
		}
		pr, err := lookup(l.PR)
		if err != nil {
			continue // can't tell; it stays in the stack
		}
		switch pr.State {
		case "CLOSED":
			errs = append(errs, a.leaveStack(l, TrainSuperseded, "its PR "+l.PR+" was closed"))
		case "MERGED":
			if pr.BaseRefName == "" || pr.BaseRefName == a.Cfg.Base {
				errs = append(errs, a.leaveStack(l, TrainMerged, "its PR "+l.PR+" was merged into "+a.Cfg.Base))
				continue
			}
			if !fetched {
				_, _ = trainGit(a.Root, "fetch", "--quiet", "origin", "+refs/heads/"+a.Cfg.Base+":refs/remotes/origin/"+a.Cfg.Base)
				fetched = true
			}
			if a.inBase(l, a.baseRef()) {
				errs = append(errs, a.leaveStack(l, TrainMerged,
					fmt.Sprintf("its PR %s was merged into %s, which reached %s", l.PR, pr.BaseRefName, a.Cfg.Base)))
				continue
			}
			errs = append(errs, a.reland(l, pr.BaseRefName))
		default:
			info[l.PR] = pr
		}
	}
	return info, errors.Join(errs...)
}

// leaveStack moves a landed task out of the stack into state, keeping its
// landed range as the note.
func (a *App) leaveStack(l landedTask, state, why string) error {
	if err := a.Store.SetTrain(l.ID, state, l.rangeNote(), false); err != nil {
		return err
	}
	a.Store.Event(l.ID, "unstacked", state+": "+why)
	msg := fmt.Sprintf("%s left the PR stack (%s): %s. Saddle won't touch its PR again.", l.ID, state, why)
	if state == TrainSuperseded {
		msg += " The next restack drops its commits from " + a.Cfg.Integration + "."
	}
	return a.Notify(OrchestratorID, store.NoticeInfo, msg)
}

// reland handles a PR merged into the stacked branch into instead of base:
// the task keeps its place in the stack but loses that PR, so the next prs
// opens a fresh one and puts into back on its own landed commit.
func (a *App) reland(l landedTask, into string) error {
	if err := a.Store.SetField(l.ID, "pr", ""); err != nil {
		return err
	}
	// prs pushes with a lease; let it see what the merge left on into.
	_, _ = trainGit(a.Root, "fetch", "--quiet", "origin", "+refs/heads/"+into+":refs/remotes/origin/"+into)
	a.Store.Event(l.ID, "pr_merged_into_stack", l.PR+" → "+into)
	return a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
		"%s's PR %s was merged into %s instead of %s, so its work isn't on %s. "+
			"Saddle detached that PR from %s: the next prs re-lands it as a fresh PR and puts %s back on its own landed commit. "+
			"Run prs; nothing needs fixing by hand.", l.ID, l.PR, into, a.Cfg.Base, a.Cfg.Base, l.ID, into))
}

// inBase reports whether ref has all of l's landed work, by patch-id or, as
// after a squash merge, by the content of every file it changed.
func (a *App) inBase(l landedTask, ref string) bool {
	if l.From == "" || l.From == l.To {
		return false
	}
	out, err := gitx.Run(a.Root, "cherry", ref, l.To, l.From)
	if err == nil && !strings.Contains(out, "+") {
		return true
	}
	return a.alreadyApplied(l.From, l.To, ref)
}

// healBranches recreates the local branches of stacked tasks that were
// deleted, at the commit the train landed.
func (a *App) healBranches(stack []landedTask) {
	var refs []string
	for _, l := range stack {
		refs = append(refs, "refs/heads/"+l.Branch)
	}
	tips, err := gitx.ResolveCommits(a.Root, refs)
	if err != nil {
		return
	}
	for _, l := range stack {
		if _, ok := tips["refs/heads/"+l.Branch]; ok || l.To == "" || l.Lost != "" {
			continue
		}
		if _, err := trainGit(a.Root, "update-ref", "refs/heads/"+l.Branch, l.To, ""); err == nil {
			a.Store.Event(l.ID, "branch_restored", l.Branch+" at "+short(l.To))
		}
	}
}

// pushLanded pushes a landed commit to its branch on origin, leased on what
// saddle last saw there. When GitHub deleted the branch (as it does after a
// merge), the stale lease is dropped and the branch pushed fresh.
func (a *App) pushLanded(branch, sha string) error {
	push := func() error {
		_, err := trainGit(a.Root, "push", "--force-with-lease=refs/heads/"+branch, "origin", sha+":refs/heads/"+branch)
		return err
	}
	err := push()
	if err == nil {
		return nil
	}
	if out, lerr := gitx.Run(a.Root, "ls-remote", "origin", "refs/heads/"+branch); lerr != nil || out != "" {
		return err
	}
	_, _ = trainGit(a.Root, "update-ref", "-d", "refs/remotes/origin/"+branch)
	return push()
}

// Unstack takes a landed task out of the PR stack for good, by task id, PR
// URL or PR number (#12). Its train entry becomes superseded and the next
// restack drops its commits from integration. It is the self-service fix for
// a task the stack should no longer carry.
func (a *App) Unstack(ref string) (store.Task, error) {
	t, err := a.taskByRef(ref)
	if err != nil {
		return t, err
	}
	unlock, err := a.lockTrain()
	if err != nil {
		return t, err
	}
	defer unlock()
	all, err := a.landedAll()
	if err != nil {
		return t, err
	}
	for _, l := range all {
		if l.ID != t.ID {
			continue
		}
		if l.State != store.TrainOK {
			return t, nil // already out
		}
		return t, a.leaveStack(l, TrainSuperseded, "unstacked by hand")
	}
	return t, fmt.Errorf("%s hasn't landed, so it isn't in the stack", t.ID)
}

// taskByRef finds a task by id, PR URL, or PR number with or without '#'.
func (a *App) taskByRef(ref string) (store.Task, error) {
	ref = strings.TrimSpace(ref)
	if t, err := a.Store.Task(ref); err == nil {
		return t, nil
	}
	n := strings.TrimPrefix(ref, "#")
	if i := strings.LastIndex(n, "/pull/"); i >= 0 {
		n = n[i+len("/pull/"):]
	}
	ts, err := a.Store.Tasks()
	if err != nil {
		return store.Task{}, err
	}
	for _, t := range ts {
		if t.PR != "" && (t.PR == ref || strings.HasSuffix(t.PR, "/pull/"+n)) {
			return t, nil
		}
	}
	return store.Task{}, fmt.Errorf("no task or PR %q", ref)
}

// AckFlag acknowledges the stack's at-risk flag: it stops freezing prs and
// land and its labels come off, until a check finds a different first broken
// layer or the stack checks clean.
func (a *App) AckFlag() (StackFlag, error) {
	f, ok, err := a.Flag()
	if err != nil {
		return f, err
	}
	if !ok {
		return f, errors.New("the stack isn't flagged; there is nothing to ack")
	}
	f.Acked = true
	if err := a.SetFlag(f); err != nil {
		return f, err
	}
	a.Store.Event(f.Task, "stack_ack", f.Cause)
	return f, nil
}

// Requeue puts a landed task whose work integration lacks back in the train,
// recreating its branch and worktree if they are gone, so it lands again.
func (a *App) Requeue(id string) error {
	unlock, err := a.lockTrain()
	if err != nil {
		return err
	}
	defer unlock()
	t, err := a.Store.Task(id)
	if err != nil {
		return err
	}
	all, err := a.landedAll()
	if err != nil {
		return err
	}
	var landed *landedTask
	for i := range all {
		if all[i].ID == id {
			landed = &all[i]
		}
	}
	if landed == nil {
		return fmt.Errorf("%s hasn't landed; call done to queue it", id)
	}
	if !gitx.BranchExists(a.Root, t.Branch) {
		if landed.To == "" {
			return fmt.Errorf("%s's branch %s is gone and saddle can't tell what it landed", id, t.Branch)
		}
		if _, err := trainGit(a.Root, "update-ref", "refs/heads/"+t.Branch, landed.To, ""); err != nil {
			return err
		}
	}
	out, err := gitx.Run(a.Root, "cherry", a.Cfg.Integration, t.Branch)
	if err != nil {
		return err
	}
	if !strings.Contains(out, "+") {
		return fmt.Errorf("%s's work is already on %s (by patch-id); nothing to requeue", id, a.Cfg.Integration)
	}
	if !a.checkedOut(t) {
		if _, err := os.Stat(t.Worktree); err == nil {
			return fmt.Errorf("%s's worktree %s exists but doesn't have %s checked out", id, t.Worktree, t.Branch)
		}
		_, _ = gitx.Run(a.Root, "worktree", "prune")
		if _, err := gitx.Run(a.Root, "worktree", "add", t.Worktree, t.Branch); err != nil {
			return err
		}
	}
	if err := errors.Join(a.Store.Enqueue(id), a.Store.SetStatus(id, store.Done)); err != nil {
		return err
	}
	a.Store.Event(id, "requeued", landed.rangeNote())
	return nil
}
