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

// Done marks a task finished and queues its branch in the merge train. It
// runs the repo's gate inline, however long it takes: the CLI's way. The
// MCP done tool uses DoneAsync.
func (a *App) Done(task, summary string) error {
	t, err := a.doneReady(task)
	if err != nil {
		return err
	}
	if err := a.lintDone(t); err != nil {
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
	if err != nil {
		return DoneJob{}, err
	}
	g := a.LintGate()
	state, tree := hooksState(t.Worktree), cleanTree(t.Worktree)
	if g.Cmd == "" || state == "" || tree == "" || passedLint(state, tree, g) {
		return DoneJob{}, a.Done(task, summary)
	}
	lock, id, ok, err := takeDoneJob(state, task)
	if err != nil {
		return DoneJob{}, a.Done(task, summary)
	}
	if !ok {
		return DoneJob{ID: id, Running: true, Joined: true}, nil
	}
	a.Store.Event(task, "done_gate_started", id+" "+g.Cmd)
	a.doneJobs.Add(1)
	go func() {
		defer a.doneJobs.Done()
		defer lock.Close()
		a.runDoneJob(t, id, tree, summary)
	}()
	return DoneJob{ID: id, Running: true}, nil
}

// WaitDoneJobs waits for the done gates this process started.
func (a *App) WaitDoneJobs() { a.doneJobs.Wait() }

// runDoneJob is the background half of DoneAsync: gate, then queue or
// tell the agent why not.
func (a *App) runDoneJob(t store.Task, id, tree, summary string) {
	fail := func(event, text string) {
		a.Store.Event(t.ID, event, id)
		_ = a.Notify(t.ID, store.NoticeAction, fmt.Sprintf("done gate %s: %s", id, text))
	}
	err := a.lintDone(t)
	if errors.Is(err, ErrGateInterrupted) {
		fail("done_gate_interrupted", "saddle stopped the gate before it finished. Your branch is not queued; call the saddle done tool again.")
		return
	}
	if err != nil {
		fail("done_gate_failed", err.Error())
		return
	}
	if now, _ := gitx.Run(t.Worktree, "rev-parse", "HEAD^{tree}"); now != tree {
		fail("done_gate_stale", "your branch moved while the gate checked it, so it is not queued. Commit everything, then call the saddle done tool again.")
		return
	}
	if err := a.finishDone(t, summary); err != nil {
		fail("done_gate_failed", "the gate passed but queueing failed: "+err.Error()+"\nCall the saddle done tool again.")
		return
	}
	_ = a.Notify(t.ID, store.NoticeInfo, fmt.Sprintf("done gate %s passed; your branch is queued in the merge train. You can stop now.", id))
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

// doneReady checks t can be done at all: a worker with a clean worktree
// and commits to land.
func (a *App) doneReady(task string) (store.Task, error) {
	t, err := a.Store.Task(task)
	if err != nil {
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
	n, err := gitx.CommitsBetween(t.Worktree, a.Cfg.Integration, "HEAD")
	if err != nil {
		return t, err
	}
	if n == 0 {
		return t, errors.New("branch has no commits beyond the integration branch; nothing to land")
	}
	return t, nil
}

// finishDone marks t done and queues it, once its gate passed.
func (a *App) finishDone(t store.Task, summary string) error {
	task := t.ID
	if summary != "" {
		if err := a.Store.SetField(task, "summary", summary); err != nil {
			return err
		}
	}
	if err := a.Store.SetStatus(task, store.Done); err != nil {
		return err
	}
	if err := a.Store.Enqueue(task); err != nil {
		return err
	}
	a.Store.Event(task, "done", summary)
	advisory := ""
	if !a.taskAdapter(t).Hooks() {
		advisory = a.advisoryClaims(t)
	}
	return a.Notify(OrchestratorID, store.NoticeAction,
		fmt.Sprintf("%s %q is done and queued in the merge train: %s\nRun the saddle land tool when you're ready.%s", task, t.Title, summary, advisory))
}
