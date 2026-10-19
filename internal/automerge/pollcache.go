package automerge

import (
	"fmt"
	"sync"
	"time"
)

// Poll back-off (#665): a PR whose state hasn't changed in QuietAfter is
// read less often, half as often as it has been quiet, up to MaxPollGap; any
// change in what is read resets it to every tick. Merged and closed PRs are
// not read again.
const (
	// DefaultQuietAfter is how long a PR must be unchanged before it backs off.
	DefaultQuietAfter = 5 * time.Minute
	// DefaultMaxPollGap caps the wait between reads of a quiet PR.
	DefaultMaxPollGap = 10 * time.Minute
)

type polled struct {
	pr        PR
	sig       string    // what the last read said, to spot a change
	changedAt time.Time // when sig last differed
	readAt    time.Time
}

// pollCache remembers each open PR's last read and the PRs found done.
type pollCache struct {
	mu   sync.Mutex
	prs  map[string]polled
	done map[string]string // PR url -> MERGED or CLOSED
}

func signature(p PR) string {
	return fmt.Sprint(p.State, p.Draft, p.Mergeable, p.MergeState, p.Base, p.HeadSHA, p.Checks, p.Labels)
}

func (w *Watcher) quietAfter() time.Duration {
	if w.QuietAfter > 0 {
		return w.QuietAfter
	}
	return DefaultQuietAfter
}

func (w *Watcher) maxPollGap() time.Duration {
	if w.MaxPollGap > 0 {
		return max(w.MaxPollGap, w.interval())
	}
	return max(DefaultMaxPollGap, w.interval())
}

// gap is how long to wait between reads of an entry: 0 until it has been
// quiet for QuietAfter, then half its quiet time, capped at MaxPollGap.
func (w *Watcher) gap(e polled) time.Duration {
	quiet := w.now().Sub(e.changedAt)
	if quiet < w.quietAfter() {
		return 0
	}
	return min(max(quiet/2, w.interval()), w.maxPollGap())
}

// pollGap is the current gap for url; 0 if unread or still changing.
func (w *Watcher) pollGap(url string) time.Duration {
	w.poll.mu.Lock()
	defer w.poll.mu.Unlock()
	e, ok := w.poll.prs[url]
	if !ok {
		return 0
	}
	return w.gap(e)
}

// cached returns the last read of url when it is not yet due, and whether
// the PR is finished (merged or closed) and need not be read at all.
func (w *Watcher) cached(url string) (pr PR, hit, finished bool) {
	w.poll.mu.Lock()
	defer w.poll.mu.Unlock()
	if _, ok := w.poll.done[url]; ok {
		return PR{}, false, true
	}
	e, ok := w.poll.prs[url]
	if !ok || w.now().Sub(e.readAt) >= w.gap(e) {
		return PR{}, false, false
	}
	return e.pr, true, false
}

// remember records a fresh read of url.
func (w *Watcher) remember(url string, pr PR) {
	w.poll.mu.Lock()
	defer w.poll.mu.Unlock()
	if pr.State != "" && pr.State != "OPEN" {
		if w.poll.done == nil {
			w.poll.done = map[string]string{}
		}
		w.poll.done[url] = pr.State
		delete(w.poll.prs, url)
		return
	}
	if w.poll.prs == nil {
		w.poll.prs = map[string]polled{}
	}
	now := w.now()
	sig := signature(pr)
	e, ok := w.poll.prs[url]
	if !ok || e.sig != sig {
		e.changedAt = now
	}
	e.pr, e.sig, e.readAt = pr, sig, now
	w.poll.prs[url] = e
}

// resetPolls makes every PR due on the next tick, after a merge or collapse
// moved the stack under them.
func (w *Watcher) resetPolls() {
	w.poll.mu.Lock()
	defer w.poll.mu.Unlock()
	w.poll.prs = nil
}
