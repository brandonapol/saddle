package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// Done-gate environment failures (#785). A done gate that fails on the
// machine (a disk quota, no temp space, the OOM killer) is not the
// worker's to fix: it can't free the owner's disk, so it loops on done or
// gives up. Instead saddle sweeps its own scratch, tells the orchestrator
// once, holds the task as done-pending-env and re-runs the gate itself with
// backoff, and tells the worker to stop calling done and wait.

// EventDonePendingEnv is a done gate that failed on the environment: the
// task waits for saddle's retry. Data: the signature and when it retries.
const EventDonePendingEnv = "done_pending_env"

// EventDoneEnvRetry is saddle re-running a done-pending-env task's gate.
const EventDoneEnvRetry = "done_env_retry"

// envRetryMax caps the backoff between retries.
const envRetryMax = 30 * time.Minute

// gateEnvPause is the pause before done's lint gate retries an environment
// failure, times the attempt; tests shorten it.
var gateEnvPause = time.Second

// envRetryBackoff is the wait before retry n+1: 1m, 2m, 4m… capped.
func envRetryBackoff(n int) time.Duration {
	d := time.Minute
	for range n {
		d *= 2
		if d >= envRetryMax {
			return envRetryMax
		}
	}
	return d
}

// DoneEnvError is done's lint gate failing on the environment, not the
// branch.
type DoneEnvError struct {
	Cmd     string
	Problem GateEnvProblem
	Out     string
	Hint    string // what fills the temp dirs, if saddle could tell
}

func (e *DoneEnvError) Error() string {
	return fmt.Sprintf("done could not check your tree: the repo's gate `%s` failed on the environment (%q), not on your branch. Free %s.%s\n%s",
		e.Cmd, e.Problem.Signature, e.Problem.Free, e.Hint, tail(e.Out, 10))
}

// PendingEnv is a task whose done gate failed on the environment, waiting
// for saddle's retry.
type PendingEnv struct {
	Task      string    `json:"task"`
	Summary   string    `json:"summary,omitempty"` // its done call's
	Signature string    `json:"signature,omitempty"`
	Since     time.Time `json:"since"`
	Attempts  int       `json:"attempts"` // retries so far
	Next      time.Time `json:"next"`
}

var pendingEnvMu sync.Mutex // guards the pending file within a process

func (a *App) pendingEnvPath() string { return a.stateDir("done-pending-env.json") }

// pendingEnv reads the done-pending-env tasks, by id.
func (a *App) pendingEnv() map[string]PendingEnv {
	m := map[string]PendingEnv{}
	if b, err := os.ReadFile(a.pendingEnvPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (a *App) writePendingEnv(m map[string]PendingEnv) error {
	p := a.pendingEnvPath()
	if len(m) == 0 {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// setPendingEnv edits task's entry, making it when there is none.
func (a *App) setPendingEnv(task string, edit func(*PendingEnv)) error {
	pendingEnvMu.Lock()
	defer pendingEnvMu.Unlock()
	m := a.pendingEnv()
	p, ok := m[task]
	if !ok {
		p = PendingEnv{Task: task, Since: time.Now()}
	}
	edit(&p)
	m[task] = p
	return a.writePendingEnv(m)
}

// clearPendingEnv drops task's entry: its branch queued, or it finished.
func (a *App) clearPendingEnv(task string) {
	pendingEnvMu.Lock()
	defer pendingEnvMu.Unlock()
	m := a.pendingEnv()
	if _, ok := m[task]; !ok {
		return
	}
	delete(m, task)
	_ = a.writePendingEnv(m)
}

// envSweep frees what saddle itself left on the disk before a retry: its
// scratch, stale gate temp dirs, and temp dirs and Go build caches under
// the repo's .saddle/tmp*. Tests replace it: it would sweep the machine's
// scratch, not theirs.
var envSweep = func(a *App, task string) {
	if testing.Testing() {
		return
	}
	res := a.SweepScratch(false, 0)
	sweepStaleTemp(a.GateTmpdir(), time.Now().Add(-time.Hour))
	n := a.sweepRepoTmp(time.Now().Add(-time.Hour))
	a.Store.Event(task, EventScratchSwept, fmt.Sprintf("done gate failed on the environment: %d scratch entries, %d repo temp dirs", len(res.Removed), n))
}

// sweepRepoTmp removes Go build caches and stale test and build temp dirs
// under <root>/.saddle/tmp*, where older saddles and agents' GOCACHE put
// them. It returns how many it removed.
func (a *App) sweepRepoTmp(cutoff time.Time) int {
	dirs, _ := filepath.Glob(filepath.Join(a.Root, ".saddle", "tmp*"))
	n := 0
	for _, d := range dirs {
		es, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range es {
			name := strings.ToLower(e.Name())
			cache := strings.HasPrefix(name, "gocache") || strings.HasPrefix(name, "go-cache") || name == "go-build-cache"
			temp := strings.HasPrefix(name, "test") || strings.HasPrefix(name, "go-build") || strings.HasPrefix(name, "gate-")
			if !e.IsDir() || (!cache && !temp) {
				continue
			}
			if !cache {
				if fi, err := e.Info(); err != nil || !fi.ModTime().Before(cutoff) {
					continue
				}
			}
			if removeTree(filepath.Join(d, e.Name())) == nil {
				n++
			}
		}
	}
	return n
}

// envWait is what the worker hears when its done gate failed on the
// environment.
func envWait(job string, p GateEnvProblem, next time.Time) string {
	what := "your done gate"
	if job != "" {
		what = "done gate " + job
	}
	return fmt.Sprintf("%s failed on the machine (%q), not on your branch. This is not yours to fix: saddle swept its scratch, told the orchestrator, "+
		"and re-runs the gate itself (next at %s, then with backoff). Stop calling done and wait; you'll get a notice when your branch is queued. "+
		"Your branch needs no change for it.", what, p.Signature, next.Local().Format("15:04"))
}

// holdDoneForEnv handles a done gate that failed on the environment: sweep,
// schedule saddle's own retry, tell the orchestrator on the first failure
// of an outage, and tell the worker to wait. It returns what the worker
// was told.
func (a *App) holdDoneForEnv(t store.Task, job, summary string, e *DoneEnvError) string {
	envSweep(a, t.ID)
	now := time.Now()
	var p PendingEnv
	first := false
	_ = a.setPendingEnv(t.ID, func(q *PendingEnv) {
		if q.Next.IsZero() {
			first = true
		} else {
			q.Attempts++
		}
		if summary != "" {
			q.Summary = summary
		}
		q.Signature = e.Problem.Signature
		q.Next = now.Add(envRetryBackoff(q.Attempts))
		p = *q
	})
	a.Store.Event(t.ID, EventDonePendingEnv, fmt.Sprintf("%s; retry %d at %s", p.Signature, p.Attempts+1, p.Next.Local().Format("15:04")))
	msg := envWait(job, e.Problem, p.Next)
	if !first {
		return msg
	}
	_ = a.Notify(OrchestratorID, store.NoticeAction, fmt.Sprintf(
		"%s %q called done, but the repo's gate `%s` failed on the machine, not the branch: %q. Free %s.%s "+
			"Saddle swept its own scratch and holds %s as done-pending-env: it re-runs the gate itself with backoff (next at %s) and queues the branch once it passes. "+
			"The worker was told to wait.",
		t.ID, t.Title, e.Cmd, e.Problem.Signature, e.Problem.Free, e.Hint, t.ID, p.Next.Local().Format("15:04")))
	_ = a.Notify(t.ID, store.NoticeAction, msg)
	return msg
}

// DonePendingEnvTick re-runs the done gate of every done-pending-env task
// whose backoff ran out at now, and drops the ones that finished or were
// queued meanwhile. It returns the tasks it retried.
func (a *App) DonePendingEnvTick(now time.Time) ([]string, error) {
	pendingEnvMu.Lock()
	m := a.pendingEnv()
	pendingEnvMu.Unlock()
	if len(m) == 0 {
		return nil, nil
	}
	train, err := a.Store.Train()
	if err != nil {
		return nil, err
	}
	queued := map[string]bool{}
	for _, e := range train {
		if e.State == store.Queued || e.State == store.OnHold {
			queued[e.Task] = true
		}
	}
	var acted []string
	var errs []error
	for id, p := range m {
		t, err := a.Store.Task(id)
		if err != nil || t.Role != store.RoleWorker || store.Finished(t.Status) || queued[id] {
			a.clearPendingEnv(id)
			continue
		}
		if now.Before(p.Next) {
			continue
		}
		// Hold the next retry back while this one runs; a gate that fails
		// on the environment again moves it on with the backoff.
		errs = append(errs, a.setPendingEnv(id, func(q *PendingEnv) { q.Next = now.Add(envRetryBackoff(q.Attempts)) }))
		a.Store.Event(id, EventDoneEnvRetry, fmt.Sprintf("retry %d after %s", p.Attempts+1, p.Signature))
		job, err := a.DoneAsync(id, p.Summary)
		if err != nil {
			a.Store.Event(id, EventDoneEnvRetry, "can't: "+oneLine(err.Error()))
			continue
		}
		if !job.Running {
			a.clearPendingEnv(id) // queued, or nothing to land
		}
		acted = append(acted, id)
	}
	return acted, errors.Join(errs...)
}
