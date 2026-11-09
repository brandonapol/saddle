package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/retry"
	"github.com/brandonapol/saddle/internal/store"
)

// Publishing on done (ADR 0012 rules 2-4, #933). Once a task's gate passed
// in its worktree, done:
//
//  1. rebases the branch onto its base (<remote>/main, or the branch of the
//     unmerged task it runs after). A conflict goes back to the task's own
//     agent, which is still alive; the task stays running.
//  2. checks every commit in base..branch is the task's own: its
//     Saddle-Task trailer names the task, or it has no trailer and no other
//     task's branch holds it. Otherwise it refuses, naming the commits.
//  3. pushes the branch and opens its one PR against the base, or updates the
//     open one. The task's status becomes pr_open.
//
// There is no train, integration branch, pre-publish gate or commit status
// on this path: the repo's CI is the gate. Running it again at the same head
// is a no-op. A push or forge failure is retried with backoff under
// internal/retry (key "publish:<task>"), with one notice.

// Event kinds of publishing on done.
const (
	EventDonePROpened   = "done_pr_opened"  // data: url, base and head
	EventPRUpdated      = "pr_updated"      // the open PR got a new head; data: url, base and head
	EventPublishNoop    = "publish_noop"    // done again at the head already published
	EventPublishRefused = "publish_refused" // a conflict or foreign commits; the agent fixes it
	EventPublishPending = "publish_pending" // a push or forge call failed; retried with backoff
	EventPublishLocal   = "publish_local"   // no remote: the branch is kept, nothing pushed
)

// PublishRefusedError is done refusing to publish for a reason the task's
// agent fixes on its branch: a rebase conflict, or commits that aren't the
// task's own.
type PublishRefusedError struct{ Msg string }

func (e *PublishRefusedError) Error() string { return e.Msg }

// publishResult is what publishDone did.
type publishResult struct {
	URL     string
	Base    string
	Noop    bool // the head was already published
	Pending bool // a push or forge call failed; it is retried
	Local   bool // no remote to publish to
	Opened  bool // a new PR was opened (not an existing one updated)
	Legacy  bool // [legacy_train]: queued in the merge train instead
	Err     error
}

// publishKey is the retry key of task's publish.
func publishKey(task string) string { return "publish:" + task }

// publishDone rebases, checks and publishes t once its gate passed. A
// refusal is a *PublishRefusedError; a push or forge failure is not an
// error: it is recorded for retry and reported as Pending.
func (a *App) publishDone(t store.Task, summary string) (publishResult, error) {
	if summary != "" {
		if err := a.Store.SetField(t.ID, "summary", summary); err != nil {
			return publishResult{}, err
		}
		t.Summary = summary
	}
	tg := a.resolveBase(t, true)
	res := publishResult{Base: tg.PRBase}
	refuse := func(msg string) (publishResult, error) {
		a.Store.Event(t.ID, EventPublishRefused, oneLine(msg))
		return res, &PublishRefusedError{Msg: msg}
	}
	rr, taken, err := trainRebase(t.Worktree, tg.Onto, tg.From, a.Cfg.Regen)
	if err != nil {
		return refuse(fmt.Sprintf("Rebasing your branch onto %s failed, so nothing was pushed:\n%s\nFix it and call the saddle done tool again.", tg.Onto, strings.TrimSpace(rr.Output)))
	}
	if !rr.OK {
		return refuse(fmt.Sprintf("Your branch passed its gate, but rebasing it onto %s conflicts in %s, so nothing was pushed. The conflict is yours to fix on this branch:\n"+
			"1. Run `saddle sync`. It starts the rebase and stops at the conflicts.\n"+
			"2. Resolve them, keeping both sides' intent, then `git add` and `git rebase --continue`.\n"+
			"3. Run the tests, then call the saddle done tool again.", tg.Onto, strings.Join(rr.Conflicts, ", ")))
	}
	if len(taken) > 0 {
		if out, err := regenerate(t.Worktree, a.Cfg.Regen, taken); err != nil {
			return refuse(fmt.Sprintf("Your branch rebased onto %s, keeping its copy of the derived files %s, but regenerating them failed:\n%s\n%s\nFix it, commit, and call the saddle done tool again.",
				tg.Onto, strings.Join(taken, ", "), out, err))
		}
		a.Store.Event(t.ID, "train_regen", strings.Join(taken, "\n"))
	}
	if n, err := gitx.CommitsBetween(t.Worktree, tg.Onto, "HEAD"); err == nil && n == 0 {
		res.Noop = true
		return res, a.finishNoChange(t, "", "after rebasing onto "+tg.Onto+" its branch has no commits of its own; its work is already there")
	}
	if foreign, err := a.foreignCommits(t, tg.Onto); err != nil {
		return res, err
	} else if len(foreign) > 0 {
		return refuse(fmt.Sprintf("Your branch has commits in %s..%s that aren't this task's, so nothing was pushed. A PR carries only its own task's work (ADR 0012):\n%s\n"+
			"Remove them from your branch. If this task needs that work, it waits until that work merges, or is spawned after the task that owns it. Then call the saddle done tool again.",
			tg.Onto, t.Branch, strings.Join(foreign, "\n")))
	}
	return a.pushAndOpenPR(t, tg)
}

// foreignCommits lists the commits in onto..HEAD of t's worktree that
// aren't t's own, as "<sha> <subject> (why)". A commit is t's own when its
// Saddle-Task trailer names t (or a task t adopted), or it has no trailer
// and no other task's branch holds it.
func (a *App) foreignCommits(t store.Task, onto string) ([]string, error) {
	out, err := gitx.Run(t.Worktree, "log", "--no-merges", "--format=%H%x1f%s%x1f%(trailers:key=Saddle-Task,valueonly,separator=%x2C)%x1e", onto+"..HEAD")
	if err != nil {
		return nil, err
	}
	mine := map[string]bool{t.ID: true}
	if e, ok, _ := a.Store.LastTaskEvent(t.ID, EventAdopts); ok {
		mine[strings.TrimSpace(e.Data)] = true
	}
	var bad []string
	for _, rec := range strings.Split(out, "\x1e") {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 3)
		if len(f) < 3 {
			continue
		}
		sha, subject := f[0], f[1]
		var ids []string
		for _, id := range strings.Split(f[2], ",") {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			if !slices.ContainsFunc(ids, func(id string) bool { return mine[id] }) {
				bad = append(bad, fmt.Sprintf("%s %s (Saddle-Task: %s)", short(sha), subject, strings.Join(ids, ", ")))
			}
			continue
		}
		if other := a.otherTaskBranch(t, sha, mine); other != "" {
			bad = append(bad, fmt.Sprintf("%s %s (also on %s)", short(sha), subject, other))
		}
	}
	return bad, nil
}

// otherTaskBranch is a saddle task branch other than t's (and its adopted
// tasks') that holds sha, or "".
func (a *App) otherTaskBranch(t store.Task, sha string, mine map[string]bool) string {
	out, err := gitx.Run(a.Root, "for-each-ref", "--contains", sha, "--format=%(refname:short)", "refs/heads/saddle/")
	if err != nil {
		return ""
	}
	for _, b := range strings.Fields(out) {
		if b == t.Branch || b == a.Cfg.Integration {
			continue
		}
		if id, ok := a.taskOnBranch(b); ok && mine[id] {
			continue
		} else if !ok {
			continue // not a task's branch
		}
		return b
	}
	return ""
}

// pushAndOpenPR pushes t's branch and opens its PR against tg's base, or
// updates the open one. Same head as last time: a no-op.
func (a *App) pushAndOpenPR(t store.Task, tg rebaseTarget) (publishResult, error) {
	res := publishResult{Base: tg.PRBase}
	head, err := gitx.RevParse(t.Worktree, "HEAD")
	if err != nil {
		return res, err
	}
	r := a.remote()
	if r == "" {
		res.Local = true
		a.Store.Event(t.ID, EventPublishLocal, t.Branch+" "+short(head))
		return res, a.publishedLocal(t)
	}
	pushed, _ := gitx.RevParse(a.Root, "refs/remotes/"+r+"/"+t.Branch)
	if pushed == head && t.PR != "" && t.Status == store.PROpen {
		res.Noop, res.URL = true, t.PR
		a.Store.Event(t.ID, EventPublishNoop, t.PR+" "+short(head))
		return res, nil
	}
	if tg.PRBase != a.Cfg.Base {
		// A stacked PR's base must exist on the remote for the PR to open.
		// One already there is its own task's to move, not this one's.
		if have, err := trainGit(a.Root, "ls-remote", "--heads", r, "refs/heads/"+tg.PRBase); err != nil {
			return a.publishFailed(t, head, res, fmt.Errorf("ls-remote %s: %w", tg.PRBase, err))
		} else if strings.TrimSpace(have) == "" {
			if _, err := trainGit(a.Root, "push", r, "refs/heads/"+tg.PRBase+":refs/heads/"+tg.PRBase); err != nil {
				return a.publishFailed(t, head, res, fmt.Errorf("push %s: %w", tg.PRBase, err))
			}
		}
	}
	if _, err := trainGit(a.Root, "push", "--force-with-lease", r, "refs/heads/"+t.Branch+":refs/heads/"+t.Branch); err != nil {
		return a.publishFailed(t, head, res, fmt.Errorf("push %s: %w", t.Branch, err))
	}
	url, opened, err := a.ensurePR(t, tg.PRBase)
	if err != nil {
		return a.publishFailed(t, head, res, err)
	}
	res.URL, res.Opened = url, opened
	_ = a.publishRetrier().Succeed(publishKey(t.ID))
	if err := a.Store.SetField(t.ID, "pr", url); err != nil {
		return res, err
	}
	kind := EventPRUpdated
	if opened {
		kind = EventDonePROpened
	}
	a.Store.Event(t.ID, kind, url+" base="+tg.PRBase+" head="+short(head))
	return res, a.publishedPR(t)
}

// ensurePR returns t's open PR against base, retargeted if it targets
// another branch, or opens one. opened says it is new.
func (a *App) ensurePR(t store.Task, base string) (url string, opened bool, err error) {
	ctx := context.Background()
	f := a.Forge()
	if t.PR != "" {
		pr, err := f.PR(ctx, t.PR, forge.PRState|forge.PRBase|forge.PRURL)
		if err != nil {
			return "", false, err
		}
		if pr.State == forge.StateOpen {
			if pr.Base != "" && pr.Base != base {
				if err := f.SetPRBase(ctx, t.PR, base); err != nil {
					return "", false, err
				}
			}
			return t.PR, false, nil
		}
	}
	prs, err := f.ListPRs(ctx, forge.PRQuery{State: "open", Head: t.Branch, Limit: 1}, forge.PRURL|forge.PRBase)
	if err != nil {
		return "", false, err
	}
	if len(prs) > 0 && prs[0].URL != "" {
		if prs[0].Base != "" && prs[0].Base != base {
			if err := f.SetPRBase(ctx, prs[0].URL, base); err != nil {
				return "", false, err
			}
		}
		return prs[0].URL, false, nil
	}
	out, err := f.CreatePR(ctx, forge.NewPR{Base: base, Head: t.Branch, Title: t.Title, Body: a.donePRBody(t)})
	if err != nil {
		u := prURLRe.FindString(err.Error())
		if why, _ := forge.RefusalOf(err); u == "" || why != forge.PRExists {
			return "", false, err
		}
		return u, false, nil
	}
	return lastLine(out), true, nil
}

// donePRBody is t's PR body: its summary (or title), the issue it closes,
// and its Saddle-Task trailer, which a squash merge carries into base.
func (a *App) donePRBody(t store.Task) string {
	desc := strings.TrimSpace(t.Summary)
	if desc == "" {
		desc = t.Title
	}
	if t.Issue > 0 && !closesIssue(desc, t.Issue) {
		desc = withCloses(desc, []int{t.Issue})
	}
	return withTaskTrailer(desc, t.ID)
}

// publishedPR records t as published: pr_open, claims released, its agent's
// leftover processes reaped. The orchestrator hears once.
func (a *App) publishedPR(t store.Task) error {
	if err := a.Store.SetStatus(t.ID, store.PROpen); err != nil {
		return err
	}
	advisory := a.publishAdvisory(t)
	a.releaseOnPublish(t)
	cur, _ := a.Store.Task(t.ID)
	return a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf("%s %q is done: its PR %s is open against %s. CI is the gate; it merges once it's green.\nSummary: %s%s",
		t.ID, t.Title, cur.PR, a.taskBase(cur), cur.Summary, advisory))
}

// publishedLocal records t as done in a repo with no remote: its branch is
// kept, nothing is pushed.
func (a *App) publishedLocal(t store.Task) error {
	if err := a.Store.SetStatus(t.ID, store.Done); err != nil {
		return err
	}
	advisory := a.publishAdvisory(t)
	a.releaseOnPublish(t)
	return a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf("%s %q is done. This repo has no remote, so nothing was pushed: its work is on branch %s.\nSummary: %s%s",
		t.ID, t.Title, t.Branch, t.Summary, advisory))
}

// publishAdvisory names, for a hookless task, the files on its branch that
// other tasks claim: its claims were only advisory (#489).
func (a *App) publishAdvisory(t store.Task) string {
	if a.taskAdapter(t).Hooks() {
		return ""
	}
	return a.advisoryClaims(t)
}

// releaseOnPublish frees what a published task no longer needs: its claims
// (logged, as a landing did) and its tagged processes.
func (a *App) releaseOnPublish(t store.Task) {
	if cl, err := a.Store.Claims(); err == nil && len(cl[t.ID]) > 0 {
		a.Store.Event(t.ID, landedClaimsEvent, strings.Join(cl[t.ID], "\n"))
	}
	if err := a.Store.Release(t.ID); err != nil {
		a.Store.Event(t.ID, "release_failed", err.Error())
	}
	_ = a.Store.DequeuePending(t.ID) // never in the train on this path
	a.clearPendingEnv(t.ID)
	a.taskWorked(t)
	a.reapTask(t.ID)
}

// publishFailed records a failed push or forge call for retry with
// backoff. The task waits as done; the orchestrator hears once, on the
// first failure.
func (a *App) publishFailed(t store.Task, head string, res publishResult, err error) (publishResult, error) {
	res.Pending, res.Err = true, err
	at, rerr := a.publishRetrier().Fail(publishKey(t.ID), "publish "+t.ID, head, err)
	if rerr != nil {
		return res, errors.Join(err, rerr)
	}
	a.Store.Event(t.ID, EventPublishPending, oneLine(err.Error()))
	if serr := a.Store.SetStatus(t.ID, store.Done); serr != nil {
		return res, serr
	}
	if at.Tries == 1 {
		_ = a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf("%s %q passed its gate, but publishing its branch failed: %s\nSaddle retries it with backoff; nothing to do unless this repeats.",
			t.ID, t.Title, oneLine(err.Error())))
	}
	return res, nil
}

// publishRetrier is the retry policy of publishing on done.
func (a *App) publishRetrier() *retry.Retrier {
	return &retry.Retrier{Store: a.Store, Escalate: func(at retry.Attempt) {
		task := strings.TrimPrefix(at.Key, "publish:")
		a.Store.Event(task, retry.EventEscalated, at.Kind+": "+oneLine(at.LastErr))
		_ = a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf("Publishing %s failed %d times the same way (%s). Saddle keeps retrying it every %s; fix the cause.",
			task, at.N, oneLine(at.LastErr), fmtDuration(retry.SlowCadence())))
	}}
}

// PublishRetryTick retries every done task whose publish failed and is due
// at now. The sentinel runs it every cycle. It returns the tasks it
// published.
func (a *App) PublishRetryTick(now time.Time) ([]string, error) {
	ts, err := a.Store.Tasks()
	if err != nil {
		return nil, err
	}
	r := a.publishRetrier()
	var out []string
	var errs []error
	for _, t := range ts {
		if t.Role != store.RoleWorker || t.Status != store.Done {
			continue
		}
		at, ok, err := r.Store.RetryAttempt(publishKey(t.ID))
		if err != nil || !ok || now.Before(at.NextAt) {
			errs = append(errs, err)
			continue
		}
		tg := a.resolveBase(t, true)
		res, err := a.pushAndOpenPR(t, tg)
		if err != nil {
			errs = append(errs, fmt.Errorf("publish %s: %w", t.ID, err))
			continue
		}
		if !res.Pending {
			out = append(out, t.ID)
		}
	}
	return out, errors.Join(errs...)
}
