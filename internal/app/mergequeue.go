package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/prgraph"
	"github.com/brandonapol/saddle/internal/retry"
	"github.com/brandonapol/saddle/internal/store"
)

// The merge queue (ADR 0012 rule 5, #935) merges pr_open tasks' PRs into
// main, one action per tick. prgraph.Queue decides; this file reads its
// input from the store, git and the forge, and applies the one action:
//
//   - rebase: a PR behind main (or conflicting) is rebased onto main in the
//     queue's own scratch worktree, never the task's live one, and
//     force-pushed with a lease on the head it read. CI then runs again.
//   - merge: a green, mergeable PR is merged with the repo's method; its
//     task becomes merged.
//   - move: a PR stacked on its declared after, once that after merged, is
//     replayed with `git rebase --onto origin/main <old base tip>`,
//     force-pushed and retargeted to main (a plain base edit).
//   - a conflicting rebase leaves the PR untouched: the task is left for its
//     own agent to fix on its branch (#936), with one notice.
//   - main red on its own required checks pauses the queue with one notice;
//     it resumes when main is green.
//
// Every failure goes through internal/retry, keyed per action and task and
// reset when the head or main moves: nothing latches. Saddle posts no
// statuses. With [legacy_train] on, the old auto-merge watcher merges
// instead and this queue doesn't run.

// Event kinds of the merge queue.
const (
	EventQueueMerged  = "queue_merged"        // data: url, method and head
	EventQueueSeen    = "queue_seen_merged"   // the forge reports the PR merged; data: url
	EventQueueRebased = "queue_rebased"       // data: url head=<new> was=<old>
	EventQueueMoved   = "queue_retargeted"    // data: url old → main head=<new>
	EventQueueFix     = "queue_fix_in_place"  // data: head=<sha> and why
	EventQueuePaused  = "queue_paused"        // main is red; data: the checks
	EventQueueResumed = "queue_resumed"       // main is green again
	EventQueueCleaned = "queue_scratch_clean" // a rebase left half-done was aborted
	EventQueueFailed  = "queue_failed"        // an action failed; retried with backoff
	EventQueueStale   = "queue_stale"         // an action's guard no longer held; skipped
)

// QueueSettle is how long after the queue pushes a head a PR with no checks
// is still taken to be starting its CI.
const QueueSettle = 2 * time.Minute

// QueueForge is the part of the forge the merge queue uses; forge.Forge is
// one.
type QueueForge interface {
	OpenPRs(ctx context.Context) ([]forge.PR, error)
	PR(ctx context.Context, ref string, f forge.PRFields) (forge.PR, error)
	MergeSettings(ctx context.Context) (forge.MergeSettings, error)
	MergePR(ctx context.Context, ref, method, head string) error
	SetPRBase(ctx context.Context, ref, base string) error
}

// MergeQueue is the merge queue over one repo.
type MergeQueue struct {
	a   *App
	F   QueueForge
	Now func() time.Time
}

// NewMergeQueue returns the merge queue, talking to f (nil is the repo's
// forge).
func (a *App) NewMergeQueue(f QueueForge) *MergeQueue {
	if f == nil {
		f = a.Forge()
	}
	return &MergeQueue{a: a, F: f, Now: time.Now}
}

// QueueTick is what one tick saw and did.
type QueueTick struct {
	Plan prgraph.Plan `json:"plan"`
	// Applied is the one action the tick applied, if any.
	Applied *prgraph.Action `json:"applied,omitempty"`
	// Result is "ok", "conflict" (left to its task), "stale" or the error.
	Result string `json:"result,omitempty"`
}

// mqTask is a pr_open task with its PR as the forge reports it.
type mqTask struct {
	t  store.Task
	pr forge.PR
}

var errQueueStale = errors.New("stale")

// retrier is the queue's retry policy: escalations tell the orchestrator.
func (q *MergeQueue) retrier() *retry.Retrier {
	return &retry.Retrier{Store: q.a.Store, Now: q.Now, Escalate: func(at retry.Attempt) {
		q.a.Store.Event("", retry.EventEscalated, at.Kind+": "+oneLine(at.LastErr))
		_ = q.a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf("The merge queue's %s failed %d times the same way (%s). It keeps retrying every %s; fix the cause.",
			at.Kind, at.N, oneLine(at.LastErr), fmtDuration(retry.SlowCadence())))
	}}
}

// Tick reads the queue's input, plans, and applies at most one action that
// isn't backing off.
func (q *MergeQueue) Tick(ctx context.Context) (QueueTick, error) {
	var out QueueTick
	a := q.a
	r := a.remote()
	if r == "" {
		return out, nil
	}
	ts, err := a.Store.Tasks()
	if err != nil {
		return out, err
	}
	var live []store.Task
	for _, t := range ts {
		if t.Role == store.RoleWorker && t.Status == store.PROpen && t.PR != "" {
			live = append(live, t)
		}
	}
	if len(live) == 0 {
		return out, nil
	}
	rt := q.retrier()
	if _, due, err := rt.Due("queue:read", ""); err != nil || !due {
		return out, err
	}
	in, tasks, err := q.input(ctx, r, live)
	if err != nil {
		_, _ = rt.Fail("queue:read", "read", "", err)
		return out, err
	}
	_ = rt.Succeed("queue:read")
	st, _ := automerge.Load(a.automergePath())
	in.Paused = st.QueuePaused != ""
	if len(in.BaseRed) == 0 && in.Paused {
		q.resume()
		in.Paused = false
	}
	out.Plan = prgraph.Queue(in)
	for _, act := range out.Plan.Effectful() {
		key := prgraph.QueueKey(act)
		inputs := act.Guard.Head + "@" + in.BaseTip
		if _, due, err := rt.Due(key, inputs); err != nil {
			return out, err
		} else if !due {
			continue
		}
		applied := act
		out.Applied = &applied
		err := q.apply(ctx, r, in, act, tasks[act.Change])
		switch {
		case err == nil:
			out.Result = "ok"
			_ = rt.Succeed(key)
		case errors.Is(err, errQueueStale):
			out.Result = "stale"
			a.Store.Event(act.Change, EventQueueStale, act.String())
		case errors.Is(err, errQueueConflict):
			out.Result = "conflict"
			_ = rt.Succeed(key)
		default:
			out.Result = err.Error()
			a.Store.Event(act.Change, EventQueueFailed, act.String()+": "+oneLine(err.Error()))
			if _, ferr := rt.Fail(key, "queue "+string(act.Kind)+" "+act.Change, inputs, err); ferr != nil {
				return out, ferr
			}
		}
		return out, nil
	}
	return out, nil
}

// input reads the queue's input: main's tip and red checks, the open PRs,
// and each pr_open task's PR, after, order and hold.
func (q *MergeQueue) input(ctx context.Context, r string, live []store.Task) (prgraph.QueueInput, map[string]mqTask, error) {
	a := q.a
	in := prgraph.QueueInput{Now: q.Now(), Base: a.Cfg.Base, Settle: QueueSettle}
	baseRef := "refs/remotes/" + r + "/" + a.Cfg.Base
	if _, err := gitx.Run(a.Root, "fetch", "--quiet", r, "+refs/heads/"+a.Cfg.Base+":"+baseRef); err != nil {
		return in, nil, fmt.Errorf("fetch %s: %w", a.Cfg.Base, err)
	}
	tip, err := gitx.RevParse(a.Root, baseRef)
	if err != nil {
		return in, nil, err
	}
	in.BaseTip = tip
	open, err := q.F.OpenPRs(ctx)
	if err != nil {
		return in, nil, err
	}
	byURL := map[string]forge.PR{}
	for _, p := range open {
		byURL[p.URL] = p
	}
	in.BaseRed = q.baseRed(ctx)
	st, _ := automerge.Load(a.automergePath())
	in.Merging = a.Cfg.Train.AutoMerge
	if st.Enabled != nil {
		in.Merging = *st.Enabled
	}
	if gh := (&automerge.GH{Forge: q.F}); in.Merging {
		if in.Method, err = gh.MergeMethod(); err != nil {
			return in, nil, err
		}
	}
	tasks := map[string]mqTask{}
	for _, t := range live {
		p, ok := byURL[t.PR]
		if !ok {
			// Not open: merged or closed since. One read says which.
			if p, err = q.F.PR(ctx, t.PR, forge.PRState|forge.PRBase|forge.PRHeadBranch|forge.PRHead|forge.PRURL|forge.PRNumber); err != nil {
				a.Store.Event(t.ID, EventQueueFailed, "read "+t.PR+": "+oneLine(err.Error()))
				continue
			}
		}
		tasks[t.ID] = mqTask{t: t, pr: p}
		in.Changes = append(in.Changes, q.change(r, t, p, st))
	}
	return in, tasks, nil
}

// change is t's queue change.
func (q *MergeQueue) change(r string, t store.Task, p forge.PR, st automerge.State) prgraph.QueueChange {
	a := q.a
	c := prgraph.QueueChange{ID: t.ID, Seq: t.CreatedAt.UnixNano(), Branch: t.Branch, Head: p.HeadSHA, PR: p.Number,
		PRBase: p.Base, Draft: p.Draft, NeedsHuman: slices.Contains(p.Labels, automerge.NeedsHuman),
		Held: slices.Contains(st.Holds, t.ID) || slices.Contains(st.Holds, t.PR)}
	if c.PR == 0 {
		c.PR = prNumberOf(t.PR)
	}
	if i := slices.Index(st.Order, t.ID); i >= 0 {
		c.Order = i + 1
	}
	switch p.State {
	case forge.StateMerged:
		c.PRState = prgraph.PRMerged
	case forge.StateClosed:
		c.PRState = prgraph.PRClosed
	default:
		c.PRState = prgraph.PROpen
	}
	switch {
	case p.Mergeable == forge.Conflicting || p.MergeState == "DIRTY":
		c.Mergeable = prgraph.MergeConflicting
	case p.Mergeable == forge.Mergeable && p.MergeState == "BLOCKED":
		c.Mergeable = prgraph.MergeBlocked
	case p.Mergeable == forge.Mergeable:
		c.Mergeable = prgraph.MergeClean
	}
	c.Behind = p.MergeState == "BEHIND"
	switch automerge.Rollup(p.Checks) {
	case automerge.ChecksPass:
		c.Checks = prgraph.CheckSuccess
	case automerge.ChecksPending:
		c.Checks = prgraph.CheckPending
	case automerge.ChecksFail:
		c.Checks = prgraph.CheckFailure
	}
	if c.Mergeable == prgraph.MergeBlocked && c.Checks == prgraph.CheckPending {
		c.Mergeable = prgraph.MergeClean // blocked only on its pending checks
	}
	if base := a.taskBase(t); base != a.Cfg.Base {
		c.AfterBranch = base
		if dep, ok := a.taskOnBranch(base); ok {
			c.After = dep
			if d, err := a.Store.Task(dep); err == nil {
				c.AfterMerged = a.depMerged(d, "refs/remotes/"+r+"/"+a.Cfg.Base)
			}
		}
		for _, ref := range []string{"refs/heads/" + base, "refs/remotes/" + r + "/" + base} {
			if mb, err := gitx.Run(a.Root, "merge-base", ref, c.Head); err == nil && mb != "" {
				c.From = mb
				break
			}
		}
	}
	if e, ok, _ := a.Store.LastTaskEvent(t.ID, EventQueueFix); ok && strings.HasPrefix(e.Data, "head="+c.Head+" ") {
		c.FixInPlace = strings.TrimPrefix(e.Data, "head="+c.Head+" ")
	}
	if e, ok, _ := a.Store.LastTaskEvent(t.ID, EventQueueRebased, EventQueueMoved); ok && strings.Contains(e.Data, "head="+c.Head) {
		c.PushedAt = e.TS
	}
	return c
}

// baseRed names the base's own required checks that are red at its tip.
// A forge that can't say is taken as green.
func (q *MergeQueue) baseRed(ctx context.Context) []string {
	b, ok := q.F.(interface {
		BranchChecks(ctx context.Context, ref string) ([]forge.CheckRun, error)
	})
	if !ok {
		return nil
	}
	runs, err := b.BranchChecks(ctx, q.a.Cfg.Base)
	if err != nil {
		return nil
	}
	var required []string
	if p, ok := q.F.(interface {
		BranchProtection(ctx context.Context, branch string) (forge.Protection, error)
	}); ok {
		if pr, err := p.BranchProtection(ctx, q.a.Cfg.Base); err == nil {
			required = pr.Checks
		}
	}
	var red []string
	for _, c := range runs {
		if len(required) > 0 && !slices.Contains(required, c.Name) {
			continue
		}
		if automerge.Rollup([]forge.CheckRun{c}) == automerge.ChecksFail && !slices.Contains(red, c.Name) {
			red = append(red, c.Name)
		}
	}
	return red
}

// resume lifts the pause once main is green, and says so once.
func (q *MergeQueue) resume() {
	_, _ = automerge.Update(q.a.automergePath(), func(s *automerge.State) error { s.QueuePaused = ""; return nil })
	q.a.Store.Event("", EventQueueResumed, q.a.Cfg.Base)
	_ = q.a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf("%s is green again; the merge queue resumed.", q.a.Cfg.Base))
}

var errQueueConflict = errors.New("conflict")

// apply applies one action.
func (q *MergeQueue) apply(ctx context.Context, r string, in prgraph.QueueInput, act prgraph.Action, qt mqTask) error {
	a := q.a
	switch act.Kind {
	case prgraph.ActTransition:
		if err := a.Store.SetStatus(act.Change, store.Merged); err != nil {
			return err
		}
		a.Store.Event(act.Change, EventQueueSeen, qt.t.PR)
		_ = a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf("%s %q merged: its PR %s is in %s.", act.Change, qt.t.Title, qt.t.PR, a.Cfg.Base))
		return nil
	case prgraph.ActEscalate:
		why := strings.TrimPrefix(act.Cause, "base_red: ")
		if _, err := automerge.Update(a.automergePath(), func(s *automerge.State) error { s.QueuePaused = why; return nil }); err != nil {
			return err
		}
		a.Store.Event("", EventQueuePaused, why)
		return a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf("%s is red on its own checks (%s). The merge queue is paused until %s is green again; no PR is asked to fix it. Fix %s.",
			a.Cfg.Base, why, a.Cfg.Base, a.Cfg.Base))
	case prgraph.ActMerge:
		return q.merge(ctx, act, qt)
	case prgraph.ActRebase, prgraph.ActMove:
		return q.rebase(ctx, r, act, qt)
	}
	return fmt.Errorf("the merge queue can't apply %s", act)
}

// merge merges the PR with the repo's method, only at the head it read.
func (q *MergeQueue) merge(_ context.Context, act prgraph.Action, qt mqTask) error {
	a := q.a
	gh := &automerge.GH{Forge: q.F}
	if err := gh.Merge(qt.t.PR, act.Method, act.Guard.Head); err != nil {
		return err
	}
	if err := a.Store.SetStatus(act.Change, store.Merged); err != nil {
		return err
	}
	a.Store.Event(act.Change, EventQueueMerged, qt.t.PR+" "+act.Method+" "+short(act.Guard.Head))
	return a.Notify(OrchestratorID, store.NoticeInfo, fmt.Sprintf("%s %q merged: the merge queue merged its PR %s into %s.", act.Change, qt.t.Title, qt.t.PR, a.Cfg.Base))
}

// queueScratch is the queue's own worktree for rebases.
func (a *App) queueScratch() string { return a.stateDir("queue") }

// cleanQueueScratch removes the queue's scratch worktree, aborting first a
// rebase a killed process left half-done there (#904).
func (a *App) cleanQueueScratch() {
	dir := a.queueScratch()
	if _, err := os.Stat(dir); err == nil {
		if gitx.RebaseInProgress(dir) {
			_, _ = trainGit(dir, "rebase", "--abort")
			a.Store.Event("", EventQueueCleaned, dir)
		}
		_ = gitx.WorktreeRemove(a.Root, dir)
		_ = os.RemoveAll(dir)
	}
	_, _ = gitx.Run(a.Root, "worktree", "prune")
}

// rebase rebases the PR's branch onto main in the scratch worktree and
// force-pushes it with a lease on the head it read. A Move replays only
// From..head and then retargets the PR to main. A conflict leaves the PR
// untouched and the task to its own agent.
func (q *MergeQueue) rebase(ctx context.Context, r string, act prgraph.Action, qt mqTask) error {
	a := q.a
	t := qt.t
	remoteRef := "refs/remotes/" + r + "/" + t.Branch
	if _, err := gitx.Run(a.Root, "fetch", "--quiet", r, "+refs/heads/"+t.Branch+":"+remoteRef); err != nil {
		return fmt.Errorf("fetch %s: %w", t.Branch, err)
	}
	cur, err := gitx.RevParse(a.Root, remoteRef)
	if err != nil {
		return err
	}
	if cur != act.Guard.Head {
		return errQueueStale
	}
	baseRef := "refs/remotes/" + r + "/" + a.Cfg.Base
	if tip, err := gitx.RevParse(a.Root, baseRef); err != nil || tip != act.Guard.BaseTip {
		return errQueueStale
	}
	a.cleanQueueScratch()
	dir := a.queueScratch()
	if err := os.MkdirAll(a.stateDir(), 0o755); err != nil {
		return err
	}
	if _, err := gitx.Run(a.Root, "worktree", "add", "--detach", dir, cur); err != nil {
		return err
	}
	defer a.cleanQueueScratch()
	from := ""
	if act.Kind == prgraph.ActMove {
		from = act.From
	}
	rr, taken, err := trainRebase(dir, baseRef, from, a.Cfg.Regen)
	if err != nil {
		return fmt.Errorf("rebase %s onto %s: %w", t.Branch, a.Cfg.Base, err)
	}
	if !rr.OK {
		return q.fixInPlace(act, qt, fmt.Sprintf("rebasing it onto %s conflicts in %s", a.Cfg.Base, strings.Join(rr.Conflicts, ", ")))
	}
	if len(taken) > 0 {
		if out, err := regenerate(dir, a.Cfg.Regen, taken); err != nil {
			return q.fixInPlace(act, qt, fmt.Sprintf("rebasing it onto %s kept %s's copy of %s, and regenerating them failed: %s", a.Cfg.Base, a.Cfg.Base, strings.Join(taken, ", "), oneLine(out)))
		}
	}
	head, err := gitx.RevParse(dir, "HEAD")
	if err != nil {
		return err
	}
	if head != cur {
		if _, err := trainGit(dir, "push", "--force-with-lease=refs/heads/"+t.Branch+":"+cur, r, head+":refs/heads/"+t.Branch); err != nil {
			return fmt.Errorf("push %s: %w", t.Branch, err)
		}
	}
	if act.Kind == prgraph.ActMove {
		if err := q.F.SetPRBase(ctx, t.PR, a.Cfg.Base); err != nil {
			return err
		}
		if err := a.Store.SetField(t.ID, "base", a.Cfg.Base); err == nil {
			a.Store.Event(t.ID, EventBaseMoved, act.Guard.PRBase+" → "+a.Cfg.Base)
		}
		a.Store.Event(t.ID, EventQueueMoved, t.PR+" "+act.Guard.PRBase+" → "+a.Cfg.Base+" head="+head)
		return nil
	}
	a.Store.Event(t.ID, EventQueueRebased, t.PR+" head="+head+" was="+cur)
	return nil
}

// fixInPlace leaves the PR untouched for its own task to fix: the head is
// marked so the queue skips it until a new one is pushed, and the
// orchestrator hears once. Nothing is spawned (#936 reopens the task).
func (q *MergeQueue) fixInPlace(act prgraph.Action, qt mqTask, why string) error {
	a := q.a
	a.Store.Event(act.Change, EventQueueFix, "head="+act.Guard.Head+" "+why)
	_ = a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf("%s %q: its PR %s can't merge yet: %s. The PR was left untouched; the conflict is %s's to fix on its own branch (saddle sync, resolve, done). The queue skips it until its branch has a new head.",
		act.Change, qt.t.Title, qt.t.PR, why, act.Change))
	return errQueueConflict
}

// Run ticks the queue until ctx ends: soon again after an action, at the
// sentinel's pace when idle. Errors are recorded as events, each once.
func (q *MergeQueue) Run(ctx context.Context) {
	last := ""
	for {
		wait := time.Minute
		if tk, err := q.Tick(ctx); err != nil {
			if msg := err.Error(); msg != last {
				last = msg
				q.a.Store.Event("", EventQueueFailed, "tick: "+oneLine(msg))
			}
		} else {
			last = ""
			if tk.Applied != nil {
				wait = 10 * time.Second
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// QueueOrder sets the owner's merge order of task: pos 1 merges first
// (#935). It is the merge queue's counterpart of the train's queue move.
func (a *App) QueueOrder(task string, pos int) error {
	t, err := a.Store.Task(task)
	if err != nil {
		return err
	}
	if t.Status != store.PROpen {
		return fmt.Errorf("%s isn't waiting in the merge queue (it is %s)", task, t.Status)
	}
	_, err = automerge.Update(a.automergePath(), func(s *automerge.State) error {
		s.Order = slices.DeleteFunc(s.Order, func(id string) bool { return id == task })
		pos = min(max(pos, 1), len(s.Order)+1)
		s.Order = slices.Insert(s.Order, pos-1, task)
		return nil
	})
	if err == nil {
		a.Store.Event(task, "queue_move", "to "+strconv.Itoa(pos))
	}
	return err
}
