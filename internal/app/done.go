package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// Done finishes a task and publishes its branch (ADR 0012, #933): once the
// repo's gate passes in its worktree, the branch is rebased onto its base,
// checked to hold only the task's own commits, pushed, and its one PR opened
// or updated. It runs the gate inline, however long it takes: the CLI's
// way. The MCP done tool uses DoneAsync.
func (a *App) Done(task, summary string) error {
	t, err := a.doneReady(task)
	if errors.Is(err, errNothingToLand) {
		return a.finishNoChange(t, summary, "its branch has no commits "+a.resolveBase(t, false).Onto+" lacks")
	}
	if err != nil {
		return err
	}
	if err := a.lintDone(t); err != nil {
		var envErr *DoneEnvError
		if errors.As(err, &envErr) {
			// Not the worker's to fix: saddle retries it (#785).
			return errors.New(a.holdDoneForEnv(t, "", summary, envErr))
		}
		return err
	}
	return a.finishDone(t, summary)
}

// DoneJob is what a done call started (#368).
type DoneJob struct {
	// ID names the background gate; "" when done finished inline.
	ID string
	// Running is set while the gate runs in the background: the task is
	// queued, or the agent told why not, when it ends.
	Running bool
	// Joined is set when a gate for the task was already running, so this
	// call started none.
	Joined bool
	// NoChange is set when the branch had nothing to land, so the task
	// finished with no change instead of queueing (#452).
	NoChange bool
	// PR is the task's PR once done published it inline.
	PR string
}

// DoneAsync is done that doesn't hold its caller for the gate (#368). An
// MCP tool call that runs make check on a busy machine goes silent past
// Claude Code's timeout, which then retries and stacks checks. So when the
// tree needs checking, DoneAsync starts the gate in the background and
// returns its job id; the job queues the task when the gate passes, and the
// agent gets a notice either way. A done call while the task's gate runs
// joins that job instead of starting another check. Jobs are per task
// across processes (a flock in the hooks' state dir), and gates still take
// the repo hooks' lock, so they run one at a time across worktrees.
func (a *App) DoneAsync(task, summary string) (DoneJob, error) {
	t, err := a.doneReady(task)
	if errors.Is(err, errNothingToLand) {
		return DoneJob{NoChange: true}, a.Done(task, summary)
	}
	if err != nil {
		return DoneJob{}, err
	}
	// The summary, for the watchdog to queue the branch with if the agent
	// goes quiet before it is queued (#467).
	a.Store.Event(task, EventDoneCalled, summary)
	g := a.LintGate()
	state, tree := hooksState(t.Worktree), cleanTree(t.Worktree)
	if g.Cmd == "" || state == "" || tree == "" || passedLint(state, tree, g) {
		return a.doneInline(task, summary)
	}
	lock, id, ok, err := takeDoneJob(state, task)
	if err != nil {
		return a.doneInline(task, summary)
	}
	if !ok {
		return DoneJob{ID: id, Running: true, Joined: true}, nil
	}
	since := a.eventCursor()
	a.Store.Event(task, "done_gate_started", id+" "+g.Cmd)
	a.doneJobs.Add(1)
	go func() {
		defer a.doneJobs.Done()
		defer lock.Close()
		a.runDoneJob(t, id, tree, summary, since)
	}()
	return DoneJob{ID: id, Running: true}, nil
}

// doneInline is Done for DoneAsync, with the PR it published.
func (a *App) doneInline(task, summary string) (DoneJob, error) {
	if err := a.Done(task, summary); err != nil {
		return DoneJob{}, err
	}
	var job DoneJob
	if t, err := a.Store.Task(task); err == nil && t.Status == store.PROpen {
		job.PR = t.PR
	}
	return job, nil
}

// EventDoneGateDropped is a done gate that finished after its task was
// killed: the task stays killed and unqueued (#784). Data: job id and why.
const EventDoneGateDropped = "done_gate_dropped"

// finishedTask reports whether task is killed, landed or no change, so a
// background job that outlived it must leave it alone (#784).
func (a *App) finishedTask(task string) bool {
	t, err := a.Store.Task(task)
	return err == nil && store.Finished(t.Status)
}

// EventDoneCalled is a done call that got past the quick checks; data: its
// summary.
const EventDoneCalled = "done_called"

// WaitDoneJobs waits for the done gates this process started.
func (a *App) WaitDoneJobs() { a.doneJobs.Wait() }

// runDoneJob is the background half of DoneAsync: gate, then queue or
// tell the agent why not. since is the event cursor when the job started.
func (a *App) runDoneJob(t store.Task, id, tree, summary string, since int64) {
	fail := func(event, text string) {
		if a.finishedTask(t.ID) {
			// Killed while the gate ran: nobody is left to tell (#784).
			a.Store.Event(t.ID, EventDoneGateDropped, id+": "+event)
			return
		}
		a.Store.Event(t.ID, event, id)
		_ = a.Notify(t.ID, store.NoticeAction, fmt.Sprintf("done gate %s: %s", id, text))
	}
	head, _ := gitx.RevParse(t.Worktree, "HEAD")
	err := a.lintDone(t)
	if errors.Is(err, ErrGateInterrupted) {
		fail("done_gate_interrupted", "saddle stopped the gate before it finished. Your branch is not published; call the saddle done tool again.")
		return
	}
	var envErr *DoneEnvError
	if errors.As(err, &envErr) {
		if a.finishedTask(t.ID) {
			a.Store.Event(t.ID, EventDoneGateDropped, id+": "+EventDonePendingEnv)
			return
		}
		// The machine failed, not the branch: saddle retries it (#785).
		a.holdDoneForEnv(t, id, summary, envErr)
		return
	}
	if err != nil {
		fail("done_gate_failed", err.Error())
		return
	}
	if files := a.rebaseConflicts(t.ID, since); len(files) > 0 {
		fail("done_gate_failed", fmt.Sprintf("the gate passed, but work that landed on %s while it ran conflicts with your branch in %s, so it is not published. "+
			"Run `saddle sync`, resolve the conflicts keeping both sides' intent, run the tests, then call the saddle done tool again.",
			a.Cfg.Integration, strings.Join(files, ", ")))
		return
	}
	if now, _ := gitx.Run(t.Worktree, "rev-parse", "HEAD^{tree}"); now != tree {
		// The train rebasing the branch onto newer landings changes the
		// tree, not the work. The train tests the branch on the new tip
		// before it lands, so queue it rather than check it again here,
		// where every landing would restart the gate (#467, #525).
		next, ok := a.rebasedOnly(t, head)
		if !ok {
			fail("done_gate_stale", "your branch moved while the gate checked it, so it is not published. Commit everything, then call the saddle done tool again.")
			return
		}
		a.Store.Event(t.ID, "done_gate_rebased", id+" "+short(head)+" → "+short(next))
	}
	res, err := a.publishOnDone(t, summary)
	var refused *PublishRefusedError
	switch {
	case errors.Is(err, store.ErrFinished):
		// Killed (or otherwise finished) while the gate ran: it stays that
		// way and its agent is gone, so there is nobody to tell (#784).
		a.Store.Event(t.ID, EventDoneGateDropped, id+": "+err.Error())
		return
	case errors.As(err, &refused):
		// The task stays running: its own agent fixes its branch.
		fail("done_gate_failed", "the gate passed, but "+refused.Msg)
		return
	case err != nil:
		fail("done_gate_failed", "the gate passed but publishing failed: "+err.Error()+"\nCall the saddle done tool again.")
		return
	}
	_ = a.Notify(t.ID, store.NoticeInfo, fmt.Sprintf("done gate %s passed; %s You can stop now.", id, res.told()))
}

// rebaseConflicts is the files the last auto-rebase of task after event
// since conflicted in, nil when there was none or it went cleanly. Derived
// files alone don't count: the train regenerates them as it lands.
func (a *App) rebaseConflicts(task string, since int64) []string {
	es, err := a.Store.EventsSince(since)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range es {
		if e.Task != task || e.Kind != "auto_rebase" {
			continue
		}
		files = nil
		if c, ok := strings.CutPrefix(e.Data, "conflicts="); ok {
			files = strings.Split(c, ",")
		}
	}
	if allRegen(a.Cfg.Regen, files) {
		return nil
	}
	return files
}

// rebasedOnly reports whether t's branch, which was at from when its gate
// started, has only been rebased since: its worktree is clean and has no
// commit whose change from wasn't already carrying. It returns the new HEAD.
func (a *App) rebasedOnly(t store.Task, from string) (string, bool) {
	if from == "" || cleanTree(t.Worktree) == "" {
		return "", false
	}
	head, err := gitx.RevParse(t.Worktree, "HEAD")
	if err != nil {
		return "", false
	}
	// Commits on the branch now, beyond integration, whose patch isn't one
	// of from's: new work the gate never saw.
	out, err := gitx.Run(t.Worktree, "rev-list", "--count", "--no-merges", "--cherry-pick", "--right-only", from+"..."+head, "^"+a.Cfg.Integration)
	if err != nil || strings.TrimSpace(out) != "0" {
		return "", false
	}
	return head, true
}

// takeDoneJob takes task's done-job lock in state without waiting. It
// returns the held lock and a new job id, or when another done holds it,
// that job's id and false. An error means no lock could be had; the
// caller runs the gate inline. The lock is released when its holder's process
// ends, so a server that dies mid-gate leaves nothing to clean up.
func takeDoneJob(state, task string) (lock *os.File, id string, ok bool, err error) {
	if err := os.MkdirAll(state, 0o755); err != nil {
		return nil, "", false, err
	}
	path := filepath.Join(state, "done-"+task+".lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, "", false, err
	}
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		f.Close()
		b, _ := os.ReadFile(path)
		return nil, strings.TrimSpace(string(b)), false, nil
	}
	id = task + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(id+"\n"), 0)
	return f, id, true, nil
}

// errNothingToLand is doneReady's answer for a branch with no work its base
// lacks: done finishes it with no change (#452).
var errNothingToLand = errors.New("branch has no commits beyond its base; nothing to publish")

// doneReady checks t can be done at all: a worker with a clean worktree
// and commits of its own. It returns errNothingToLand, with t, when every
// commit is already on its base or there are none.
func (a *App) doneReady(task string) (store.Task, error) {
	if a.Cfg.LegacyTrain {
		return a.doneReadyOn(task, func(store.Task) string { return a.Cfg.Integration })
	}
	return a.doneReadyOn(task, func(t store.Task) string { return a.resolveBase(t, false).Onto })
}

// doneReadyOn is doneReady with the ref t's commits are counted beyond.
func (a *App) doneReadyOn(task string, base func(store.Task) string) (store.Task, error) {
	t, err := a.Store.Task(task)
	if err != nil {
		return t, err
	}
	if err := a.reloadGateConfig(); err != nil {
		return t, err
	}
	if t.Role != store.RoleWorker {
		return t, errors.New("only worker tasks can be done")
	}
	dirty, err := gitx.Dirty(t.Worktree)
	if err != nil {
		return t, err
	}
	if len(dirty) > 0 {
		return t, fmt.Errorf("worktree has uncommitted changes; commit them first:\n%s", strings.Join(dirty, "\n"))
	}
	// Commits whose patch the base already has (another task merged the
	// same fix) don't count: the rebase would drop them (#452).
	out, err := gitx.Run(t.Worktree, "rev-list", "--count", "--cherry-pick", "--right-only", base(t)+"...HEAD")
	if err != nil {
		return t, err
	}
	if strings.TrimSpace(out) == "0" {
		return t, errNothingToLand
	}
	if err := a.doneClaims(t); err != nil {
		return t, err
	}
	return t, nil
}

// finishNoChange ends t with nothing to land (#452): the issue was already
// fixed, or its work reached integration another way. It never enters the
// train or the PR stack, so no empty landing can flag the stack at risk.
func (a *App) finishNoChange(t store.Task, summary, why string) error {
	if summary != "" {
		if err := a.Store.SetField(t.ID, "summary", summary); err != nil {
			return err
		}
	}
	if err := errors.Join(
		a.Store.Dequeue(t.ID),
		a.Store.Release(t.ID),
		a.Store.SetStatus(t.ID, store.NoChange),
	); err != nil {
		return err
	}
	a.Store.Event(t.ID, "no_change", why)
	a.clearPendingEnv(t.ID)
	a.reapTask(t.ID)
	if t.Role == store.RoleWorker {
		// Nothing to land, so nothing to keep the worktree for (#803).
		if note, err := a.cleanup(t); err != nil {
			a.Store.Event(t.ID, "cleanup_failed", err.Error())
		} else if note != "" {
			a.Store.Event(t.ID, "cleanup", note)
		}
	}
	msg := fmt.Sprintf("%s %q finished with no change: %s, so there is nothing to land. It is out of the merge train and the PR stack.", t.ID, t.Title, why)
	if summary != "" {
		msg += "\nSummary: " + summary
	}
	return a.Notify(OrchestratorID, store.NoticeAction, msg)
}

// finishDone publishes t once its gate passed (ADR 0012, #933).
func (a *App) finishDone(t store.Task, summary string) error {
	_, err := a.publishOnDone(t, summary)
	return err
}

// publishOnDone is publishDone with done's bookkeeping: the done event, and a
// refusal on a killed task.
func (a *App) publishOnDone(t store.Task, summary string) (publishResult, error) {
	if a.Cfg.LegacyTrain {
		return publishResult{Legacy: true}, a.queueInTrain(t, summary)
	}
	if a.finishedTask(t.ID) {
		return publishResult{}, fmt.Errorf("%s is finished; not publishing it: %w", t.ID, store.ErrFinished)
	}
	res, err := a.publishDone(t, summary)
	if err != nil || res.Noop && res.URL == "" {
		return res, err
	}
	a.Store.Event(t.ID, "done", summary)
	return res, nil
}

// told is what the agent hears about a publish that went through.
func (r publishResult) told() string {
	switch {
	case r.Legacy:
		return "your branch is queued in the merge train."
	case r.Local:
		return "this repo has no remote, so your branch is kept as it is."
	case r.Pending:
		return "pushing your branch or opening its PR failed (" + oneLine(r.Err.Error()) + "); saddle retries it."
	case r.Noop:
		return "your branch is already published at this head: " + r.URL + "."
	case r.Opened:
		return "your PR " + r.URL + " is open against " + r.Base + "."
	}
	return "your PR " + r.URL + " is updated."
}

// DoneToTrain is the legacy done: gate, then queue the branch in the merge
// train for `saddle land`. Done no longer takes this path (ADR 0012); it is
// kept only while the train exists and is deleted with it (#938).
func (a *App) DoneToTrain(task, summary string) error {
	t, err := a.doneReadyOn(task, func(store.Task) string { return a.Cfg.Integration })
	if errors.Is(err, errNothingToLand) {
		return a.finishNoChange(t, summary, "its branch has no commits "+a.Cfg.Integration+" lacks")
	}
	if err != nil {
		return err
	}
	if err := a.lintDone(t); err != nil {
		var envErr *DoneEnvError
		if errors.As(err, &envErr) {
			return errors.New(a.holdDoneForEnv(t, "", summary, envErr))
		}
		return err
	}
	return a.queueInTrain(t, summary)
}

// queueInTrain marks t done and queues it in the legacy merge train.
func (a *App) queueInTrain(t store.Task, summary string) error {
	task := t.ID
	if summary != "" {
		if err := a.Store.SetField(task, "summary", summary); err != nil {
			return err
		}
	}
	// One transaction: a kill that lands between the status write and the
	// enqueue must not leave the killed task in the train (#784).
	if err := a.Store.FinishDone(task); err != nil {
		return err
	}
	a.Store.Event(task, "done", summary)
	a.clearPendingEnv(task)
	a.taskWorked(t) // its adapter delivered: no-reset backoff starts over (#796)
	a.reapTask(task)
	advisory := ""
	if !a.taskAdapter(t).Hooks() {
		advisory = a.advisoryClaims(t)
	}
	return a.Notify(OrchestratorID, store.NoticeAction,
		fmt.Sprintf("%s %q is done and queued in the merge train: %s\nRun the saddle land tool when you're ready.%s", task, t.Title, summary, advisory))
}

// eventCursor is the id of the newest event, for EventsSince; 0 when there
// is none or the store can't say.
func (a *App) eventCursor() int64 {
	es, err := a.Store.Events(1)
	if err != nil || len(es) == 0 {
		return 0
	}
	return es[0].ID
}
