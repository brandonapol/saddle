package app

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// No-reset limit backoff (#796). A banner that names no reset used to keep
// its adapter out for a flat hour. Restored with nothing running, the
// balanced policy picked it next, and its agent hit the limit within
// seconds: a handoff and an owner page every hour, all night. Now each
// no-reset banner before a successful run doubles the wait (1h, 2h, 4h…,
// capped at ExhaustMax), and an out-of-credits banner keeps the adapter
// out until ClearExhausted, since credits don't come back on a timer.

func (a *App) limitBackoffPath() string { return a.stateDir("adapters-backoff.json") }

func (a *App) readLimitBackoff() map[string]int {
	m := map[string]int{}
	if b, err := os.ReadFile(a.limitBackoffPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (a *App) writeLimitBackoff(m map[string]int) error {
	p := a.limitBackoffPath()
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

// limitBackoff is how long the n+1th no-reset banner in a row keeps an
// adapter out.
func limitBackoff(n int) time.Duration {
	d := ExhaustFor
	for range n {
		d *= 2
		if d >= ExhaustMax {
			return ExhaustMax
		}
	}
	return min(d, ExhaustMax)
}

// limitUntil is when label (an adapter, or adapter:model) is back after
// banner b at now: b's reset when it names one; zero (until cleared) for a
// credits banner; else the next step of label's backoff, which it takes.
func (a *App) limitUntil(label string, b usage.LimitBanner, now time.Time) time.Time {
	if until, ok := parseReset(b.Resets, now); ok {
		return until
	}
	if b.Credits {
		return time.Time{}
	}
	exhaustMu.Lock()
	defer exhaustMu.Unlock()
	m := a.readLimitBackoff()
	n := m[label]
	m[label] = n + 1
	_ = a.writeLimitBackoff(m)
	return now.Add(limitBackoff(n))
}

// limitWorked resets label's no-reset backoff: a run on it succeeded, or
// its banner cleared on its own.
func (a *App) limitWorked(label string) {
	exhaustMu.Lock()
	defer exhaustMu.Unlock()
	m := a.readLimitBackoff()
	if _, ok := m[label]; !ok {
		return
	}
	delete(m, label)
	_ = a.writeLimitBackoff(m)
}

// taskWorked resets the backoff of the adapter, and the tracked model, t
// ran on: it finished its work.
func (a *App) taskWorked(t store.Task) {
	ad := a.taskAdapter(t).Name()
	a.limitWorked(ad)
	if m, ok := a.Cfg.TracksModel(ad, t.Model); ok {
		a.limitWorked(exhaustKey(ad, m))
	}
}
