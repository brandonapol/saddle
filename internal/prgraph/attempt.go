package prgraph

import "time"

// Record folds one outcome of a into its attempt row (prev, or a fresh row
// when prev.Key is empty) and returns the new row. It is pure; the Engine
// stores the result.
//
// NextAt is when Reconcile may try the same key again: after a NeedsFix, the
// action's RetryAt; after a Publish, Move or Rebase, the settle time (check
// results are stale until then); after a failure, a capped exponential
// backoff, or the forge's rate-limit time.
func Record(prev Attempt, a Action, o Outcome, now time.Time, p Policy) Attempt {
	p = p.withDefaults()
	att := prev
	if att.Key == "" {
		att = Attempt{Key: a.AttemptKey(), Kind: string(a.Kind), Inputs: a.Guard.Head}
	}
	if o.Kind == OutcomeStale {
		return att
	}
	att.Last = o.Kind
	if !o.Kind.waiting() {
		att.Waits = 0
	}
	switch {
	case o.Kind == OutcomeOK:
		att.LastErr = ""
		switch a.Kind {
		case ActNeedsFix:
			att.N++
			att.NextAt = a.RetryAt
			if att.NextAt.IsZero() {
				att.NextAt = now.Add(p.FixWait)
			}
		case ActPublish, ActMove, ActRebase:
			att.NextAt = now.Add(p.Settle)
		case ActEscalate:
			att.Escalated = true
			att.NextAt = time.Time{}
		default:
			att.NextAt = time.Time{}
		}
	case o.Kind.waiting():
		// A forge wait is not a failure: it never counts toward a cap or an
		// escalation, only toward its own backoff.
		att.Waits++
		att.LastErr = o.Reason
		att.NextAt = now.Add(backoff(att.Waits, p))
		if o.Until.After(att.NextAt) {
			att.NextAt = o.Until
		}
		return att
	default:
		att.N++
		if att.FirstErr == "" {
			att.FirstErr = o.Reason
		}
		att.LastErr = o.Reason
		att.NextAt = now.Add(backoff(att.N, p))
	}
	return att
}

// backoff is RetryBase * 2^(n-1), capped at RetryMax.
func backoff(n int, p Policy) time.Duration {
	d := p.RetryBase
	for i := 1; i < n && d < p.RetryMax; i++ {
		d *= 2
	}
	if d > p.RetryMax {
		d = p.RetryMax
	}
	return d
}
