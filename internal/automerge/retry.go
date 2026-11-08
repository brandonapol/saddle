package automerge

import (
	"fmt"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/retry"
)

// The watcher never latches (#838, #826). A failed merge, restack or
// collapse is a retry.Attempt keyed by the action and its PR or stack, at
// the PR's head: it backs off and is tried again on a later tick. Transient
// failures (rate limits, timeouts, 5xx, a moved remote branch, a busy
// worktree) back off a little and never escalate. Anything else backs off
// 1m, 5m, 30m, then every 2h, and on the third try at the same error the
// orchestrator hears about it once and a blocker issue is filed; it still
// keeps trying, so nothing waits on a human. A new head on the PR is a new
// attempt. A merge GitHub refuses for want of the async merge API is tried
// through it on the next tick.

// EventEscalated is written once when an action keeps failing the same
// way; the blocker sweep files an issue for it.
const EventEscalated = retry.EventEscalated

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
	return forge.Transient(err) || gitTransient(err)
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

// asyncMerger is a GitHub that can merge through the async merge API
// directly; GH is one when its forge is.
type asyncMerger interface {
	MergeAsync(url, method, head string) error
}

// retrier is the watcher's retry policy over its attempts store.
func (w *Watcher) retrier() *retry.Retrier {
	if w.Attempts == nil {
		w.Attempts = retry.NewMemStore()
	}
	return &retry.Retrier{
		Store:     w.Attempts,
		Now:       w.now,
		Transient: gitTransient,
		Handled: func(r forge.Reason) bool {
			_, ok := w.GH.(asyncMerger)
			return ok && r == forge.StackedNeedsAsyncMerge
		},
		Escalate: func(a retry.Attempt) {
			w.event(w.escalating, EventEscalated, a.Kind+": "+a.LastErr)
			w.notify(true, fmt.Sprintf("Auto-merge keeps failing: %s failed %d times: %s. It keeps retrying every %s on its own; fix the cause or merge it by hand.",
				a.Kind, a.N, a.LastErr, retry.SlowCadence()))
		},
	}
}

// attempt is key's attempt at head, if any.
func (w *Watcher) attempt(key, head string) (retry.Attempt, bool) {
	a, ok, err := w.retrier().Get(key, head)
	if err != nil {
		w.event("", EventFailed, "reading retry attempt "+key+": "+err.Error())
	}
	return a, ok
}

// backingOff says why key mustn't be tried yet at head, "" when it may.
// An attempt at another head is dropped.
func (w *Watcher) backingOff(key, head string) string {
	a, due, err := w.retrier().Due(key, head)
	if err != nil {
		w.event("", EventFailed, "reading retry attempt "+key+": "+err.Error())
		return ""
	}
	if due {
		return ""
	}
	return fmt.Sprintf("retrying at %s after %d failed tries: %s", a.NextAt.Local().Format("15:04:05"), a.Tries, a.LastErr)
}

// failed records a failed try of key at head and backs it off; what names
// the action in events and the escalation.
func (w *Watcher) failed(key, task, head, what string, err error) {
	w.escalating = task
	a, rerr := w.retrier().Fail(key, what, head, err)
	w.escalating = ""
	if rerr != nil {
		w.event(task, EventFailed, fmt.Sprintf("%s failed: %v; recording the retry: %v", what, err, rerr))
		return
	}
	wait := a.NextAt.Sub(w.now())
	switch a.Outcome {
	case retry.Refused:
		w.event(task, EventWaiting, fmt.Sprintf("%s refused (%s); the next tick takes the route for it: %v", what, a.Reason, err))
	case retry.Transient, retry.RateLimited:
		w.event(task, EventWaiting, fmt.Sprintf("%s failed (try %d), retrying in %s: %v", what, a.Tries, wait.Round(time.Second), err))
	default:
		w.event(task, EventFailed, fmt.Sprintf("%s failed (try %d), retrying in %s: %v", what, a.Tries, wait.Round(time.Second), err))
	}
}

// succeeded clears key's failures.
func (w *Watcher) succeeded(key string) {
	if err := w.retrier().Succeed(key); err != nil {
		w.event("", EventFailed, "clearing retry attempt "+key+": "+err.Error())
	}
}

func mergeKey(pr string) string       { return "automerge:merge:" + pr }
func restackKey(pr string) string     { return "automerge:restack:" + pr }
func collapseKey(stack string) string { return "automerge:collapse:" + stack }
