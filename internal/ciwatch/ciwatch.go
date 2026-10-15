// Package ciwatch polls CI status for open saddle PRs and reports changes.
//
// It asks the forge (forge.Forge): one Checks call per target per poll, plus
// one FailedLog for each check that newly fails. It
// remembers the last state of every check and emits an Event only when a
// check turns red or goes back to green, so a poll that sees nothing new is
// silent.
package ciwatch

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
)

// Target is one PR or branch to watch. Task is the saddle task that owns the
// branch; it is carried through to events so the caller can route them.
type Target struct {
	Task   string
	Branch string
	// PR is a PR number or URL. When empty, the forge resolves the PR from
	// Branch.
	PR string
}

// Ref is what the forge is asked about: the PR when known, else the branch.
func (t Target) Ref() string {
	if t.PR != "" {
		return t.PR
	}
	return t.Branch
}

// Check is one CI check on a PR, as the forge reports it.
type Check struct {
	Name     string
	Workflow string
	// Bucket is the coarse state: pass, fail, pending, skipping or cancel.
	Bucket string
	// Link is the check's details URL; for GitHub Actions, the job page.
	Link string
}

// Label is "workflow / name", or just the name when there is no workflow.
func (c Check) Label() string {
	if c.Workflow == "" {
		return c.Name
	}
	return c.Workflow + " / " + c.Name
}

// Buckets.
const (
	BucketPass = "pass"
	BucketFail = "fail"
)

// Event is one CI change. The concrete types are Failed and Recovered;
// switch on them.
type Event interface {
	From() Origin
	// Message is a notice body for the agent or orchestrator, without the
	// "[saddle]" prefix.
	Message() string
}

// Origin says which target an event is about and when it was seen.
type Origin struct {
	Target
	At time.Time
}

// From returns the event's origin.
func (o Origin) From() Origin { return o }

// Failed reports a check that turned red: it was passing, unseen, or failing
// on an earlier run.
type Failed struct {
	Origin
	Check Check
	// RunURL is the workflow run page, or the check link for non-Actions checks.
	RunURL string
	// Step is the last step name in the failed log, when known.
	Step string
	// LogTail is the end of the failed job's log as plain text. Empty for
	// checks that aren't CI jobs or when the fetch failed.
	LogTail string
	// LogErr is set when the log could not be fetched.
	LogErr string
}

// Recovered reports a check that was failing and now passes.
type Recovered struct {
	Origin
	Check Check
}

// CI is the part of the forge the watcher reads; forge.Forge is one.
type CI interface {
	Checks(ctx context.Context, ref string) ([]forge.CICheck, error)
	FailedLog(ctx context.Context, link string, lines int) (forge.JobLog, error)
}

// Config configures a Watcher.
type Config struct {
	// CI is the forge. Required. One that returns forge.ErrUnsupported
	// watches nothing and reports no errors.
	CI CI
	// Targets lists what to watch, read fresh on every poll so landed and
	// closed PRs come and go. Required.
	Targets func(ctx context.Context) ([]Target, error)
	// Interval between polls in Run. Default 10 minutes.
	Interval time.Duration
	// LogLines is how many lines of a failed log to keep. Default 40.
	LogLines int
	// OnError receives forge and Targets errors. The target is zero for a
	// Targets error. Nil drops them. A target whose forge call fails keeps
	// its previous state, so a flaky forge never reads as a recovery.
	OnError func(t Target, err error)
	// Now is the clock. Default time.Now.
	Now func() time.Time
}

// Watcher polls CI and remembers the last state of each check.
type Watcher struct {
	cfg Config

	mu   sync.Mutex
	seen map[string]map[string]Seen // target ref → check label → state
}

// Seen is the remembered state of one check: its last pass or fail bucket
// and the link it had then. Pending and skipped results do not change it.
type Seen struct {
	Bucket string
	Link   string
}

// New returns a Watcher with no remembered state.
func New(cfg Config) (*Watcher, error) {
	if cfg.CI == nil || cfg.Targets == nil {
		return nil, errors.New("ciwatch: CI and Targets are required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 10 * time.Minute
	}
	if cfg.LogLines <= 0 {
		cfg.LogLines = 40
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Watcher{cfg: cfg, seen: map[string]map[string]Seen{}}, nil
}

// Run polls once now and then every Interval until ctx is done, passing each
// event to emit.
func (w *Watcher) Run(ctx context.Context, emit func(Event)) {
	t := time.NewTicker(w.cfg.Interval)
	defer t.Stop()
	for {
		for _, e := range w.Poll(ctx) {
			emit(e)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Poll checks every target once and returns the changes since the last poll.
func (w *Watcher) Poll(ctx context.Context) []Event {
	w.mu.Lock()
	defer w.mu.Unlock()
	targets, err := w.cfg.Targets(ctx)
	if err != nil {
		w.fail(Target{}, err)
		return nil
	}
	var out []Event
	live := map[string]bool{}
	for _, t := range targets {
		if ctx.Err() != nil {
			return out
		}
		ref := t.Ref()
		if ref == "" || live[ref] {
			continue
		}
		live[ref] = true
		cs, err := w.cfg.CI.Checks(ctx, ref)
		if errors.Is(err, forge.ErrUnsupported) {
			continue
		}
		if err != nil {
			w.fail(t, err)
			continue
		}
		checks := make([]Check, len(cs))
		for i, c := range cs {
			checks[i] = Check(c)
		}
		out = append(out, w.diff(ctx, t, checks)...)
	}
	for ref := range w.seen {
		if !live[ref] {
			delete(w.seen, ref)
		}
	}
	return out
}

// diff updates the remembered state for t and returns its events.
func (w *Watcher) diff(ctx context.Context, t Target, checks []Check) []Event {
	ref := t.Ref()
	prev := w.seen[ref]
	next := make(map[string]Seen, len(checks))
	origin := Origin{Target: t, At: w.cfg.Now()}
	var out []Event
	for _, c := range checks {
		key := c.Label()
		old, had := prev[key]
		switch c.Bucket {
		case BucketFail:
			next[key] = Seen{Bucket: BucketFail, Link: c.Link}
			// A new link is a new run (a push or re-run) failing again.
			if !had || old.Bucket != BucketFail || old.Link != c.Link {
				out = append(out, w.failed(ctx, origin, c))
			}
		case BucketPass:
			next[key] = Seen{Bucket: BucketPass, Link: c.Link}
			if had && old.Bucket == BucketFail {
				out = append(out, Recovered{Origin: origin, Check: c})
			}
		default:
			// Pending, skipped or cancelled: keep what we knew, so
			// fail → pending → pass still reads as a recovery.
			if had {
				next[key] = old
			}
		}
	}
	w.seen[ref] = next
	return out
}

func (w *Watcher) failed(ctx context.Context, o Origin, c Check) Failed {
	f := Failed{Origin: o, Check: c, RunURL: c.Link}
	l, err := w.cfg.CI.FailedLog(ctx, c.Link, w.cfg.LogLines)
	if l.Run != "" {
		f.RunURL = l.Run
	}
	switch {
	case errors.Is(err, forge.ErrUnsupported):
	case err != nil:
		f.LogErr = err.Error()
	default:
		f.Step, f.LogTail = l.Step, l.Tail
	}
	return f
}

func (w *Watcher) fail(t Target, err error) {
	if w.cfg.OnError != nil {
		w.cfg.OnError(t, err)
	}
}

// Snapshot returns a copy of the remembered state, keyed by target ref and
// then check label, so a caller can persist it across restarts.
func (w *Watcher) Snapshot() map[string]map[string]Seen {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make(map[string]map[string]Seen, len(w.seen))
	for ref, m := range w.seen {
		c := make(map[string]Seen, len(m))
		for k, v := range m {
			c[k] = v
		}
		out[ref] = c
	}
	return out
}

// Restore replaces the remembered state with one from Snapshot. Without it,
// a fresh Watcher reports every currently failing check once.
func (w *Watcher) Restore(s map[string]map[string]Seen) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.seen = make(map[string]map[string]Seen, len(s))
	for ref, m := range s {
		c := make(map[string]Seen, len(m))
		for k, v := range m {
			c[k] = v
		}
		w.seen[ref] = c
	}
}
