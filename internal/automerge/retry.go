package automerge

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
)

// The watcher never latches (#838, the merging half of #826). A failed
// merge, restack or collapse is an Attempt: it backs off and is tried again
// on a later tick. Transient failures (rate limits, timeouts, 5xx, a moved
// remote branch, a busy worktree) back off a little and never escalate.
// Anything else backs off 1m, 5m, 30m, then every 2h, and after
// escalateAfter tries the orchestrator hears about it once; it still keeps
// trying, so nothing waits on a human typing `saddle automerge on`. A new
// head on the PR is a new attempt.

// Attempt is one action's run of failures.
type Attempt struct {
	N         int       `json:"n"`
	Head      string    `json:"head,omitempty"` // the head it failed at; a new one resets it
	NextAt    time.Time `json:"next_at"`
	LastErr   string    `json:"last_err"`
	Transient bool      `json:"transient,omitempty"`
	Escalated bool      `json:"escalated,omitempty"`
}

// escalateAfter is how many deterministic failures in a row tell the
// orchestrator.
const escalateAfter = 3

var (
	transientBackoff = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}
	failBackoff      = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}
)

func backoff(transient bool, n int) time.Duration {
	steps := failBackoff
	if transient {
		steps = transientBackoff
	}
	return steps[min(max(n, 1), len(steps))-1]
}

// gitMarks are git failures that clear by themselves: a push that lost a
// race, a busy worktree, a dropped connection. Forge failures come typed
// (forge.Classify) and are never matched by text.
var gitMarks = []string{
	"stale info", "remote branch moved", "index.lock", "no such file or directory",
	"connection reset", "connection refused", "unexpected eof", "timed out",
}

// transient reports whether err clears by itself.
func transient(err error) bool {
	if err == nil {
		return false
	}
	return forge.Transient(err) || errors.Is(err, forge.ErrMergePending) || gitTransient(err)
}

// gitTransient reports whether err is a git failure that clears by itself.
func gitTransient(err error) bool {
	m := strings.ToLower(err.Error())
	for _, s := range gitMarks {
		if strings.Contains(m, s) {
			return true
		}
	}
	return false
}

// rateLimited reports whether err is GitHub refusing for rate limits (#680).
func rateLimited(err error) bool { return errors.Is(err, forge.ErrRateLimited) }

// backingOff says why key mustn't be tried yet at head, "" when it may.
// An attempt at another head is dropped.
func (w *Watcher) backingOff(s *State, key, head string) string {
	a, ok := s.Attempts[key]
	if !ok {
		return ""
	}
	if head != "" && a.Head != "" && a.Head != head {
		delete(s.Attempts, key)
		return ""
	}
	if !w.now().Before(a.NextAt) {
		return ""
	}
	return fmt.Sprintf("retrying at %s after %d failed tries: %s", a.NextAt.Local().Format("15:04:05"), a.N, a.LastErr)
}

// failed records a failed try of key at head and backs it off. A
// deterministic failure tells the orchestrator once, on its
// escalateAfter-th try in a row.
func (w *Watcher) failed(s *State, key, task, head, what string, err error) {
	if s.Attempts == nil {
		s.Attempts = map[string]Attempt{}
	}
	a := s.Attempts[key]
	if head != "" && a.Head != head {
		a = Attempt{}
	}
	a.N++
	a.Head, a.LastErr, a.Transient = head, err.Error(), transient(err)
	wait := backoff(a.Transient, a.N)
	if until, ok := forge.RateLimitUntil(err); ok && until.After(w.now().Add(wait)) {
		wait = until.Sub(w.now())
	}
	a.NextAt = w.now().Add(wait)
	kind := EventFailed
	if a.Transient {
		kind = EventWaiting
	}
	w.event(task, kind, fmt.Sprintf("%s failed (try %d), retrying in %s: %v", what, a.N, wait.Round(time.Second), err))
	if !a.Transient && a.N >= escalateAfter && !a.Escalated {
		a.Escalated = true
		w.notify(true, fmt.Sprintf("Auto-merge keeps failing: %s failed %d times: %v. It keeps retrying every %s on its own; fix the cause or merge it by hand.",
			what, a.N, err, failBackoff[len(failBackoff)-1]))
	}
	s.Attempts[key] = a
}

// succeeded clears key's failures.
func (w *Watcher) succeeded(s *State, key string) {
	delete(s.Attempts, key)
}

func mergeKey(pr string) string       { return "merge:" + pr }
func restackKey(pr string) string     { return "restack:" + pr }
func collapseKey(stack string) string { return "collapse:" + stack }
