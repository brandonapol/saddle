package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/prgraph"
	"github.com/brandonapol/saddle/internal/store"
)

// Fix in place (ADR 0012 rule 6, #936). A problem on a published task's PR
// stays on that PR and with that task:
//
//   - a conflict (the merge queue's rebase onto main, or the forge saying the
//     PR conflicts) or red CI on a pr_open task's PR reopens the same task:
//     its status becomes fixing, with the reason stored on the task
//     ("conflict: <files>" or "ci: <failed checks> <link>");
//   - if the task's agent is alive (its window is up and it showed a sign of
//     life within [stall] hung_after) it is sent the reason. Otherwise an
//     agent is started as that task in its own worktree on its own branch:
//     its session resumed when the adapter can, else a fresh one with the
//     original prompt, the reason and the PR link. Before it starts, the
//     local branch adopts the PR's remote head when the queue moved it and
//     the worktree is clean;
//   - the agent fixes and calls done, whose normal publish force-pushes the
//     same PR and makes the task pr_open again;
//   - each reopen counts against [merge] fix_attempts. The count resets when
//     the PR goes green or the owner retries. A problem past it labels the PR
//     needs-human, tells the owner once and leaves the task gave_up, until
//     the label is removed or `saddle retry` sends it back to pr_open;
//   - a fixing PR holds nothing but itself and the PRs stacked on it: no
//     file freezes, no claims, no reservations. While main is red no fix
//     starts;
//   - a PR closed without merging kills its task (killed_by pr_closed); its
//     worktree and branch are left alone.
//
// No new task, branch or PR is ever made for a fix. The merge queue's tick
// runs this pass, since it already reads every PR.

// Event kinds of fixing in place.
const (
	EventFixOpened  = "fix_opened"         // data: attempt n/N and the reason
	EventFixTold    = "fix_told"           // the task's live agent was sent the reason; data: how it was woken
	EventFixStarted = "fix_started"        // an agent was started as the task; data: how and window
	EventFixAdopted = "fix_adopted_remote" // the local branch was reset to the PR's head; data: old → new
	EventFixKept    = "fix_kept_local"     // the PR's head moved but the local branch was kept; data: why
	EventFixGreen   = "fix_green"          // the PR went green: the fix count is reset
	EventFixGaveUp  = "fix_gave_up"        // fixes ran out; data: the reason
	EventFixRetry   = "fix_retry"          // the fix count was reset by the owner; data: how
	EventFixFailed  = "fix_failed"         // a fix step failed; retried next tick
	EventPRClosed   = "pr_closed"          // the PR was closed without merging; the task is killed
)

// prLabeler labels and unlabels PRs; forge.Forge is one.
type prLabeler interface {
	AddLabel(ctx context.Context, ref, label string) error
	RemoveLabel(ctx context.Context, ref, label string) error
}

// fixAttempts is [merge] fix_attempts.
func (a *App) fixAttempts() int {
	if n := a.Cfg.Merge.FixAttempts; n > 0 {
		return n
	}
	return 2
}

// fixTracked reports whether t is a published task whose PR the fix pass
// follows.
func fixTracked(t store.Task) bool {
	if t.Role != store.RoleWorker || t.PR == "" {
		return false
	}
	switch t.Status {
	case store.PROpen, store.Fixing, store.GaveUp:
		return true
	}
	return false
}

// fixPass runs fix in place over the tracked tasks, whose PRs in has read.
// It returns the tasks it took out of the queue's hands (reopened, killed,
// given up).
func (q *MergeQueue) fixPass(in prgraph.QueueInput, r string, tasks map[string]mqTask) map[string]bool {
	a := q.a
	now := q.Now()
	mainRed := len(in.BaseRed) > 0 || in.Paused
	ids := make([]string, 0, len(tasks))
	for id := range tasks {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	taken := map[string]bool{}
	fail := func(id, what string, err error) {
		if err == nil {
			return
		}
		data := what + ": " + oneLine(err.Error())
		if e, ok, _ := a.Store.LastTaskEvent(id, EventFixFailed); !ok || e.Data != data {
			a.Store.Event(id, EventFixFailed, data)
		}
	}
	for _, id := range ids {
		qt := tasks[id]
		t, p := qt.t, qt.pr
		switch p.State {
		case forge.StateClosed:
			taken[id] = true
			fail(id, "pr closed", a.prClosed(t))
			continue
		case forge.StateMerged:
			if t.Status != store.PROpen { // pr_open is the queue's to transition
				taken[id] = true
				fail(id, "merged", a.fixMerged(t))
			}
			continue
		}
		human := slices.Contains(p.Labels, automerge.NeedsHuman)
		switch t.Status {
		case store.GaveUp:
			if !human {
				fail(id, "retry", a.resetFix(t, "the "+automerge.NeedsHuman+" label was removed from "+t.PR, nil))
			}
		case store.Fixing:
			if !mainRed && !a.agentAlive(t, now) {
				fail(id, "restart", a.restartFix(t, now, q.labeler()))
			}
		case store.PROpen:
			if human {
				continue // the owner's; the queue leaves it alone too
			}
			checks := automerge.Rollup(p.Checks)
			if checks == automerge.ChecksPass && t.FixAttempts > 0 {
				if err := a.Store.SetFixAttempts(id, 0); err == nil {
					a.Store.Event(id, EventFixGreen, t.PR+" head="+short(p.HeadSHA))
				}
			}
			if mainRed {
				continue
			}
			reason := ""
			switch {
			case checks == automerge.ChecksFail:
				reason = ciReason(p)
			case !in.Merging && p.Base == a.Cfg.Base && (p.Mergeable == forge.Conflicting || p.MergeState == "DIRTY"):
				// The queue rebases conflicting PRs only while it merges; with
				// merging off, the forge's word is checked with git.
				if files := a.conflictFiles(r, p); len(files) > 0 {
					reason = "conflict: " + strings.Join(files, ", ")
				}
			}
			if reason != "" {
				taken[id] = true
				fail(id, "reopen", a.reopenForFix(t, reason, now, q.labeler()))
			}
		}
	}
	return taken
}

// labeler is what the queue labels PRs with: its forge when it can, else
// the repo's.
func (q *MergeQueue) labeler() prLabeler {
	if l, ok := q.F.(prLabeler); ok {
		return l
	}
	return q.a.Forge()
}

// ciReason is the fix reason for p's red checks: their names and where to
// read them.
func ciReason(p forge.PR) string {
	var names []string
	for _, c := range p.Checks {
		if automerge.Rollup([]forge.CheckRun{c}) != automerge.ChecksFail {
			continue
		}
		n := c.Name
		if n == "" {
			n = c.Context
		}
		if c.Workflow != "" && n != "" {
			n = c.Workflow + " / " + n
		}
		if n != "" && !slices.Contains(names, n) {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		names = []string{"a check"}
	}
	link := p.URL
	if link != "" {
		link = " (" + strings.TrimSuffix(link, "/") + "/checks)"
	}
	return "ci: " + strings.Join(names, ", ") + " failed" + link
}

// conflictFiles names the files p's head conflicts in against its base on
// remote r, by a merge in git's object store (no worktree). None means git
// merges it cleanly.
func (a *App) conflictFiles(r string, p forge.PR) []string {
	if r == "" || p.Head == "" {
		return nil
	}
	ref := "refs/remotes/" + r + "/" + p.Head
	if _, err := gitx.Run(a.Root, "fetch", "--quiet", r, "+refs/heads/"+p.Head+":"+ref); err != nil {
		return nil
	}
	out, err := gitx.Run(a.Root, "merge-tree", "--write-tree", "--name-only", "--no-messages", "refs/remotes/"+r+"/"+p.Base, ref)
	if err == nil {
		return nil
	}
	var files []string
	for i, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); i > 0 && l != "" && !slices.Contains(files, l) {
			files = append(files, l)
		}
	}
	return files
}

// reopenForFix reopens t, whose PR has a problem, on its own branch: fixing
// with reason, and its agent told or one started. Past [merge]
// fix_attempts it gives up instead.
func (a *App) reopenForFix(t store.Task, reason string, now time.Time, l prLabeler) error {
	max := a.fixAttempts()
	if t.FixAttempts >= max {
		return a.giveUpFix(t, reason, l)
	}
	n := t.FixAttempts + 1
	if err := a.Store.Reopen(t.ID, reason, n); err != nil {
		return err
	}
	a.Store.Event(t.ID, EventFixOpened, fmt.Sprintf("attempt %d/%d: %s", n, max, reason))
	t.Status, t.FixReason, t.FixAttempts = store.Fixing, reason, n
	how, err := a.startFix(t, now)
	if err != nil {
		how = "starting its agent failed (" + oneLine(err.Error()) + "); saddle tries again"
	}
	return errors.Join(err, a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf(
		"%s %q: its PR %s needs a fix (%s). Saddle reopened %s on its own branch, attempt %d of %d: %s. Nothing to do; the same PR is updated when it calls done.",
		t.ID, t.Title, t.PR, reason, t.ID, n, max, how)))
}

// restartFix starts a new agent for a fixing task whose agent is gone. It
// counts as an attempt.
func (a *App) restartFix(t store.Task, now time.Time, l prLabeler) error {
	max := a.fixAttempts()
	if t.FixAttempts >= max {
		return a.giveUpFix(t, t.FixReason+"; its agent stopped without a fix", l)
	}
	n := t.FixAttempts + 1
	if err := a.Store.SetFixAttempts(t.ID, n); err != nil {
		return err
	}
	a.Store.Event(t.ID, EventFixOpened, fmt.Sprintf("attempt %d/%d: its agent is gone; %s", n, max, t.FixReason))
	t.FixAttempts = n
	_, err := a.startFix(t, now)
	return err
}

// startFix gets t's fix going: its live agent is sent the reason, or an
// agent is started as t in its worktree. It says which.
func (a *App) startFix(t store.Task, now time.Time) (string, error) {
	alive := a.agentAlive(t, now)
	a.adoptRemoteHead(t, alive)
	if alive {
		if err := a.Store.Notify(t.ID, store.NoticeAction, fixNote(t, a.fixAttempts())); err != nil {
			return "", err
		}
		how := a.typeWake(t)
		a.Store.Event(t.ID, EventFixTold, how)
		return "its agent was told", nil
	}
	if a.ownWindow(t) {
		// Idle past hung_after: it is gone, and two agents must never share
		// the worktree.
		if err := a.Tmux.KillWindow(t.Window); err != nil {
			a.Store.Event(t.ID, EventFixFailed, "close its old window: "+oneLine(err.Error()))
		}
	}
	note := fixNote(t, a.fixAttempts())
	r, err := a.relaunch(t, note, note)
	if err != nil {
		return "", err
	}
	how := "a fresh agent was started in its worktree"
	if r.Session != "" {
		how = "its session " + r.Session + " was resumed in its worktree"
	}
	a.Store.Event(t.ID, EventFixStarted, how+" window "+r.Window)
	return how, nil
}

// fixNote is what t's agent is told about the fix it is reopened for.
func fixNote(t store.Task, max int) string {
	what := "Its CI is red. Read the failing checks (the link above), reproduce the failure locally, write a failing test if one is missing, and fix it."
	if strings.HasPrefix(t.FixReason, "conflict") {
		what = "It conflicts with its base. Run `saddle sync`: it rebases this branch and stops at the conflicts. Resolve them keeping both sides' intent, `git add` them and `git rebase --continue`."
		if files, picks, ok := strings.Cut(strings.TrimPrefix(t.FixReason, "conflict: "), flattenPicks); ok {
			// saddle flatten moved the branch onto main (#937).
			what = "saddle flatten moved this task off the old stacked PRs: your branch now holds only your own work on main. Cherry-pick these in order, resolve the conflict in " +
				files + ", then call done. The commits: " + picks + "."
		}
	}
	return fmt.Sprintf("[saddle] Your PR %s needs a fix: %s (attempt %d of %d). %s "+
		"Fix it here, on branch %s in this worktree: never a new branch, task or PR. Run the tests, commit, and call the saddle done tool. "+
		"Done force-pushes this same PR; once it goes green the merge queue merges it.",
		t.PR, t.FixReason, t.FixAttempts, max, what, t.Branch)
}

// agentLifeKinds are events that show t's agent alive, or just started.
var agentLifeKinds = append(slices.Clone(heartbeatKinds), "spawn", "resumed", EventHungTakeover, EventFixOpened, EventFixStarted)

// agentAlive reports whether t's agent is alive: its window is up, the hung
// watch hasn't flagged it, and it showed a sign of life within [stall]
// hung_after. An agent that exited, or sat idle past that, is gone (#923).
func (a *App) agentAlive(t store.Task, now time.Time) bool {
	if !a.ownWindow(t) {
		return false
	}
	if hung, err := a.FlaggedHung(); err == nil && slices.ContainsFunc(hung, func(h HungWorker) bool { return h.Task == t.ID }) {
		return false
	}
	e, ok, err := a.Store.LastTaskEvent(t.ID, agentLifeKinds...)
	if err != nil || !ok {
		return false
	}
	window := a.StallConfig().HungAfter
	if window <= 0 {
		window = DefaultHungAfter
	}
	return now.Sub(e.TS) < window
}

// adoptRemoteHead resets t's local branch to its PR's head on the remote
// when the merge queue moved it there (a rebase or retarget) and the local
// branch has nothing of its own: a later done must not push the old head
// back. A dirty worktree, a live agent's unpublished commits, or a failed
// fetch leave it as it is.
func (a *App) adoptRemoteHead(t store.Task, alive bool) {
	r := a.remote()
	if r == "" || t.Branch == "" {
		return
	}
	ref := "refs/remotes/" + r + "/" + t.Branch
	if _, err := gitx.Run(a.Root, "fetch", "--quiet", r, "+refs/heads/"+t.Branch+":"+ref); err != nil {
		return
	}
	remote, err := gitx.RevParse(a.Root, ref)
	if err != nil {
		return
	}
	local, err := gitx.RevParse(t.Worktree, "HEAD")
	if err != nil || local == remote {
		return
	}
	if cur, err := gitx.Run(t.Worktree, "symbolic-ref", "--quiet", "--short", "HEAD"); err != nil || cur != t.Branch {
		a.Store.Event(t.ID, EventFixKept, "its worktree isn't on "+t.Branch)
		return
	}
	if dirty, err := gitx.Dirty(t.Worktree); err != nil || len(dirty) > 0 {
		a.Store.Event(t.ID, EventFixKept, "its worktree has uncommitted changes")
		return
	}
	// Commits on the local branch whose patch the remote head lacks are work
	// the agent hasn't published yet: keep them.
	out, err := gitx.Run(t.Worktree, "cherry", ref, "HEAD")
	if err != nil {
		return
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "+") {
			a.Store.Event(t.ID, EventFixKept, "it has commits the PR's head lacks")
			return
		}
	}
	if e, ok, _ := a.Store.LastTaskEvent(t.ID, EventFlattenMoved, EventPRUpdated, EventDonePROpened, EventQueueRebased, EventQueueMoved); ok && e.Kind == EventFlattenMoved {
		// saddle flatten moved the branch onto main on purpose; the PR's
		// old stacked head is what it moved off (#937).
		return
	}
	_ = alive // an agent at its prompt sees the new head in git log; nothing else to tell it
	if _, err := trainGit(t.Worktree, "reset", "--quiet", "--hard", remote); err != nil {
		a.Store.Event(t.ID, EventFixKept, "reset failed: "+oneLine(err.Error()))
		return
	}
	a.Store.Event(t.ID, EventFixAdopted, short(local)+" → "+short(remote))
}

// giveUpFix stops fixing t: its PR is labeled needs-human, its agent is
// stopped (worktree and branch kept) and the owner is told once. It stays
// gave_up until the label is removed or the owner runs saddle retry.
func (a *App) giveUpFix(t store.Task, reason string, l prLabeler) error {
	if l == nil {
		l = a.Forge()
	}
	if err := l.AddLabel(context.Background(), t.PR, automerge.NeedsHuman); err != nil {
		return fmt.Errorf("label %s %s: %w", t.PR, automerge.NeedsHuman, err)
	}
	if err := a.Store.SetStatus(t.ID, store.GaveUp); err != nil {
		return err
	}
	_ = a.Store.SetField(t.ID, "fix_reason", reason)
	if a.ownWindow(t) {
		_ = a.Tmux.KillWindow(t.Window)
	}
	a.reapTask(t.ID)
	a.Store.Event(t.ID, EventFixGaveUp, reason)
	return a.tellOwner(fmt.Sprintf("%s %q: its PR %s still needs a fix after %d attempts (%s), so saddle stopped and labeled it %s. "+
		"Its work is on branch %s. Fix it yourself, or remove the label (or run saddle retry %s) to let saddle try again.",
		t.ID, t.Title, t.PR, t.FixAttempts, reason, automerge.NeedsHuman, t.Branch, t.ID))
}

// resetFix zeroes t's fix count and, when it gave up, puts it back to
// pr_open for the queue. With l set the needs-human label is removed first.
func (a *App) resetFix(t store.Task, why string, l prLabeler) error {
	if l != nil && t.PR != "" {
		if err := l.RemoveLabel(context.Background(), t.PR, automerge.NeedsHuman); err != nil {
			return fmt.Errorf("unlabel %s %s: %w", t.PR, automerge.NeedsHuman, err)
		}
	}
	if err := a.Store.SetFixAttempts(t.ID, 0); err != nil {
		return err
	}
	if t.Status == store.GaveUp {
		if err := a.Store.SetStatus(t.ID, store.PROpen); err != nil {
			return err
		}
	}
	a.Store.Event(t.ID, EventFixRetry, why)
	return a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf("%s %q: %s, so its fix count starts again and its PR %s is back in the merge queue.", t.ID, t.Title, why, t.PR))
}

// Retry is `saddle retry <task>` (#936): the owner sends a task saddle gave
// up fixing back to pr_open, with the needs-human label removed and its fix
// count reset. On a pr_open or fixing task it only resets the count.
func (a *App) Retry(task string) error { return a.retry(task, a.Forge()) }

func (a *App) retry(task string, l prLabeler) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	switch t.Status {
	case store.GaveUp, store.PROpen, store.Fixing:
	default:
		return fmt.Errorf("%s is %s; only a task with an open PR (pr_open, fixing or gave_up) can be retried", task, t.Status)
	}
	if t.PR == "" {
		return fmt.Errorf("%s has no PR", task)
	}
	return a.resetFix(t, "the owner ran saddle retry", l)
}

// fixMerged records the merge of a fixing or gave_up task's PR, which
// someone merged on the forge.
func (a *App) fixMerged(t store.Task) error {
	if err := a.Store.SetStatus(t.ID, store.Merged); err != nil {
		return err
	}
	if a.ownWindow(t) {
		_ = a.Tmux.KillWindow(t.Window)
	}
	a.reapTask(t.ID)
	a.Store.Event(t.ID, EventQueueSeen, t.PR)
	return a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf("%s %q merged: its PR %s is in %s.", t.ID, t.Title, t.PR, a.Cfg.Base))
}

// prClosed kills t, whose PR was closed without merging (#936): its agent
// stops and its claims go, but its worktree and branch are left alone.
func (a *App) prClosed(t store.Task) error {
	if err := a.Store.KillBy(t.ID, store.KilledByPRClosed); err != nil {
		return err
	}
	if err := a.Store.Release(t.ID); err != nil {
		a.Store.Event(t.ID, "release_failed", err.Error())
	}
	if a.ownWindow(t) {
		_ = a.Tmux.KillWindow(t.Window)
	}
	a.reapTask(t.ID)
	a.Store.Event(t.ID, EventPRClosed, t.PR)
	return a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf("%s %q: its PR %s was closed without merging, so the task is killed. Its branch %s and worktree are kept.",
		t.ID, t.Title, t.PR, t.Branch))
}

// errNoRepairs refuses a repair of orig: with legacy_train off, a problem
// on a PR is fixed in place by its own task (ADR 0012 rule 6, #936).
func errNoRepairs(orig string) error {
	return fmt.Errorf("saddle no longer spawns repairs: a conflict or red CI on %s's PR reopens %s itself on its own branch (fix in place, #936); saddle retry %s resets its fix count", orig, orig, orig)
}

// noRepairSpawn refuses, with legacy_train off, a spawn that would carry
// another task's work: a repair (For) or a CI fix task. Fixes happen in
// place (#936).
func (a *App) noRepairSpawn(r SpawnReq) error {
	if a.Cfg.LegacyTrain {
		return nil
	}
	if r.For != "" {
		return errNoRepairs(r.For)
	}
	if strings.HasPrefix(r.Title, ciFixPrefix) {
		return fmt.Errorf("spawn refused: %q would be a CI fix task; red CI on a PR reopens its own task (fix in place, #936)", r.Title)
	}
	return nil
}
