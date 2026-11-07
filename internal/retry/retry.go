// Package retry is saddle's one retry-and-escalate primitive (#826, ADR
// 0004): no failure latches. Every action saddle takes on its own (a merge,
// a restack, a resume, a repair spawn) is an Attempt under a key. A failed
// try is classified and backed off:
//
//   - Transient and RateLimited (forge.ErrTransient, forge.ErrRateLimited, a
//     caller's own transient test): back off 30s up to 5m, or until the
//     forge's reset time. These never count toward escalation.
//   - Refused, by a forge rule the caller handles (Retrier.Handled): due at
//     once, so the next try routes to the handler for that reason (async
//     merge for forge.StackedNeedsAsyncMerge). Unhandled refusals are
//     Deterministic.
//   - Deterministic: back off 1m, 5m, 30m, then 2h for good. After
//     EscalateAfter tries in a row at the same error the Retrier escalates
//     once (a blocker issue and an orchestrator notice, by the caller's
//     Escalate) and keeps retrying at the slow cadence, so nothing waits on
//     a human.
//
// A change to the key's inputs (head SHA, base SHA, config) resets the
// attempt. Attempts live in one sqlite table (store.RetryStore).
package retry

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
)

// Outcome classifies a failed try.
type Outcome string

const (
	Transient     Outcome = "transient"
	RateLimited   Outcome = "rate_limited"
	Refused       Outcome = "refused" // by a rule the caller handles
	Deterministic Outcome = "deterministic"
)

// Attempt is one key's run of failed tries.
type Attempt struct {
	Key     string
	Kind    string  // the action: merge, restack, collapse, resume, ...
	Outcome Outcome // of the last failed try
	Reason  string  // the forge refusal reason, when Outcome is Refused
	// N is the deterministic tries in a row at the same error; only these
	// count toward escalation.
	N int
	// Tries is every failed try since the attempt began; it steps the
	// transient backoff.
	Tries     int
	NextAt    time.Time
	FirstErr  string
	LastErr   string
	Sig       string // the normalized error N counts tries of
	Inputs    string // what the key was tried at; a change resets the attempt
	Escalated bool   // escalated for Sig already
	UpdatedAt time.Time
}

// EventEscalated is the events-table kind a caller writes when it
// escalates an attempt: data is the attempt's Kind, ": ", then its LastErr.
// The blocker sweep files a blocker issue for each (app/blockerissue.go).
const EventEscalated = "retry_escalated"

// EscalateAfter is how many deterministic tries in a row at the same error
// escalate.
const EscalateAfter = 3

var (
	// TransientBackoff steps a transient failure's wait.
	TransientBackoff = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}
	// DeterministicBackoff steps a deterministic failure's wait; the last
	// step repeats for good.
	DeterministicBackoff = []time.Duration{time.Minute, 5 * time.Minute, 30 * time.Minute, 2 * time.Hour}
)

// SlowCadence is how often a deterministic failure keeps retrying once it
// has escalated.
func SlowCadence() time.Duration { return DeterministicBackoff[len(DeterministicBackoff)-1] }

func step(steps []time.Duration, n int) time.Duration {
	return steps[min(max(n, 1), len(steps))-1]
}

// Store keeps attempts by key. store.Store implements it over the
// retry_attempts table; MemStore is one in memory.
type Store interface {
	RetryAttempt(key string) (Attempt, bool, error)
	PutRetryAttempt(a Attempt) error
	DeleteRetryAttempt(key string) error
}

// Retrier applies the policy above over a Store.
type Retrier struct {
	Store Store
	Now   func() time.Time
	// Transient adds the caller's own transient failures (git races, a
	// busy worktree) to the forge's.
	Transient func(error) bool
	// Handled says which forge refusals the caller routes to a handler.
	Handled func(forge.Reason) bool
	// Escalate is told once per (key, error), on its EscalateAfter-th try.
	Escalate func(Attempt)
}

func (r *Retrier) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// Classify types err under r's policy, with the refusal reason if any.
func (r *Retrier) Classify(err error) (Outcome, string) {
	switch {
	case errors.Is(err, forge.ErrRateLimited):
		return RateLimited, ""
	case forge.Transient(err), errors.Is(err, forge.ErrMergePending), r.Transient != nil && r.Transient(err):
		return Transient, ""
	}
	if why, ok := forge.RefusalOf(err); ok && r.Handled != nil && r.Handled(why) {
		return Refused, why.String()
	}
	return Deterministic, ""
}

// Get is key's attempt, dropped (and reported absent) when it was made at
// other inputs.
func (r *Retrier) Get(key, inputs string) (Attempt, bool, error) {
	a, ok, err := r.Store.RetryAttempt(key)
	if err != nil || !ok {
		return Attempt{}, false, err
	}
	if a.Inputs != inputs {
		return Attempt{}, false, r.Store.DeleteRetryAttempt(key)
	}
	return a, true, nil
}

// Due is key's attempt at inputs and whether it may be tried now. A key
// with no attempt is due.
func (r *Retrier) Due(key, inputs string) (Attempt, bool, error) {
	a, ok, err := r.Get(key, inputs)
	if err != nil {
		return a, false, err
	}
	return a, !ok || !r.now().Before(a.NextAt), nil
}

// Fail records a failed try of key at inputs, backs it off and escalates
// once when a deterministic error repeats EscalateAfter times.
func (r *Retrier) Fail(key, kind, inputs string, err error) (Attempt, error) {
	a, ok, gerr := r.Get(key, inputs)
	if gerr != nil {
		return a, gerr
	}
	now := r.now()
	if !ok {
		a = Attempt{Key: key, Inputs: inputs, FirstErr: err.Error()}
	}
	a.Kind, a.Tries, a.LastErr, a.UpdatedAt = kind, a.Tries+1, err.Error(), now
	a.Outcome, a.Reason = r.Classify(err)
	var wait time.Duration
	switch a.Outcome {
	case Transient, RateLimited:
		wait = step(TransientBackoff, a.Tries)
		if until, ok := forge.RateLimitUntil(err); ok && until.After(now.Add(wait)) {
			wait = until.Sub(now)
		}
	case Refused:
		wait = 0
	default:
		if sig := Signature(err); sig != a.Sig {
			a.Sig, a.N, a.Escalated = sig, 0, false
		}
		a.N++
		wait = step(DeterministicBackoff, a.N)
	}
	a.NextAt = now.Add(wait)
	escalate := a.Outcome == Deterministic && a.N >= EscalateAfter && !a.Escalated
	if escalate {
		a.Escalated = true
	}
	if err := r.Store.PutRetryAttempt(a); err != nil {
		return a, err
	}
	if escalate && r.Escalate != nil {
		r.Escalate(a)
	}
	return a, nil
}

// Succeed clears key's attempt.
func (r *Retrier) Succeed(key string) error { return r.Store.DeleteRetryAttempt(key) }

var sigNoise = regexp.MustCompile(`https?://\S+|\b[0-9a-f]{7,40}\b|\d+`)

// Signature is err normalized for "the same error": its refusal reason
// when the forge refused it, else its text lowercased without URLs, SHAs
// and numbers.
func Signature(err error) string {
	if why, ok := forge.RefusalOf(err); ok {
		return "refused:" + why.String()
	}
	s := sigNoise.ReplaceAllString(strings.ToLower(err.Error()), "")
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 160 {
		s = s[:160]
	}
	return s
}

// MemStore is a Store in memory, for tests and callers without a state.db.
type MemStore struct{ m map[string]Attempt }

func NewMemStore() *MemStore { return &MemStore{m: map[string]Attempt{}} }

func (s *MemStore) RetryAttempt(key string) (Attempt, bool, error) {
	a, ok := s.m[key]
	return a, ok, nil
}

func (s *MemStore) PutRetryAttempt(a Attempt) error { s.m[a.Key] = a; return nil }

func (s *MemStore) DeleteRetryAttempt(key string) error { delete(s.m, key); return nil }
