package usage

import (
	"sort"
	"time"
)

// Window lengths for plan-limit estimates.
const (
	FiveHours = 5 * time.Hour
	Week      = 7 * 24 * time.Hour
)

// DefaultWarnAt is the fraction of a cap at which a window turns Warn when
// Limits.WarnAt is unset.
const DefaultWarnAt = 0.8

// Cap bounds usage in one window. A zero field is unlimited; when both are
// set, the window is as full as the fuller of the two.
type Cap struct {
	Tokens int64   `json:"tokens,omitempty" toml:"tokens"` // all token kinds, cache included
	USD    float64 `json:"usd,omitempty" toml:"usd"`       // $-equivalent at list prices
}

func (c Cap) unlimited() bool { return c.Tokens <= 0 && c.USD <= 0 }

// Limits is the user's plan-limit configuration. The zero value tracks usage
// with no caps.
type Limits struct {
	FiveHour Cap `json:"five_hour" toml:"five_hour"`
	Weekly   Cap `json:"weekly" toml:"weekly"`
	// WeeklyReset is any instant at which the weekly window resets (e.g. the
	// last reset shown by the plan). Windows are WeeklyReset + k weeks. Zero
	// means a rolling 7-day window.
	WeeklyReset time.Time `json:"weekly_reset,omitzero" toml:"weekly_reset"`
	// WarnAt is the fraction of a cap (0 < WarnAt <= 1) at which a window
	// turns Warn. Zero means DefaultWarnAt.
	WarnAt float64 `json:"warn_at,omitempty" toml:"warn_at"`
	// PauseLaunches asks callers to hold new launches while any window is
	// Over. See Estimate.ShouldPauseLaunches.
	PauseLaunches bool `json:"pause_launches,omitempty" toml:"pause_launches"`
	// Prices overrides DefaultPrices by model id prefix.
	Prices map[string]Price `json:"prices,omitempty" toml:"prices"`
}

func (l Limits) warnAt() float64 {
	if l.WarnAt <= 0 || l.WarnAt > 1 {
		return DefaultWarnAt
	}
	return l.WarnAt
}

// State is a window's threshold state. Larger is worse.
type State int

// Threshold states.
const (
	OK State = iota
	Warn
	Over
)

func (s State) String() string {
	switch s {
	case Warn:
		return "warn"
	case Over:
		return "over"
	}
	return "ok"
}

// MarshalText renders the state as "ok", "warn" or "over".
func (s State) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// WindowEstimate is usage against the cap of one window.
type WindowEstimate struct {
	Name  string    `json:"name"`  // "5h" or "weekly"
	Start time.Time `json:"start"` // usage is counted in (Start, now] for rolling windows, [Start, now] for anchored
	// ResetsAt is when usage next drops: the end of an anchored window, or
	// when the oldest counted record leaves a rolling one. Zero for an empty
	// rolling window.
	ResetsAt time.Time     `json:"resets_at,omitzero"`
	ResetIn  time.Duration `json:"reset_in"`

	Tokens   Tokens  `json:"tokens"`
	USD      float64 `json:"usd"`
	Messages int64   `json:"messages"`
	// Unpriced lists models whose usage counts toward Tokens but adds $0.
	Unpriced []string `json:"unpriced,omitempty"`

	Cap          Cap     `json:"cap"`
	Unlimited    bool    `json:"unlimited"`
	TokenPercent float64 `json:"token_percent"` // fraction of Cap.Tokens, 0 if unset
	USDPercent   float64 `json:"usd_percent"`   // fraction of Cap.USD, 0 if unset
	Percent      float64 `json:"percent"`       // max of the two; 1 means at cap
	State        State   `json:"state"`
}

// LimitEstimate is the plan-limit picture at one instant.
type LimitEstimate struct {
	Now      time.Time      `json:"now"`
	FiveHour WindowEstimate `json:"five_hour"`
	Weekly   WindowEstimate `json:"weekly"`
	State    State          `json:"state"` // worst window
	pause    bool
}

// ShouldPauseLaunches reports whether new launches should be held: the user
// opted in with Limits.PauseLaunches and some window is Over. Running agents
// are never affected.
func (e LimitEstimate) ShouldPauseLaunches() bool { return e.pause && e.State == Over }

type sample struct {
	t        time.Time
	model    string
	tokens   Tokens
	messages int64
}

// Estimate computes plan-limit usage from records as of now. Records after
// now are ignored. It is pure: the same inputs always give the same output.
func Estimate(recs []Record, l Limits, now time.Time) LimitEstimate {
	s := make([]sample, len(recs))
	for i, r := range recs {
		s[i] = sample{r.Time, r.Model, r.Tokens, 1}
	}
	return estimate(s, l, now)
}

// EstimateBuckets is Estimate over persisted minute buckets, dating each
// bucket at its minute.
func EstimateBuckets(bs []Bucket, l Limits, now time.Time) LimitEstimate {
	s := make([]sample, len(bs))
	for i, b := range bs {
		s[i] = sample{b.Minute, b.Model, b.Tokens, b.Messages}
	}
	return estimate(s, l, now)
}

func estimate(s []sample, l Limits, now time.Time) LimitEstimate {
	five := rolling("5h", FiveHours, l.FiveHour, s, l, now)
	var wk WindowEstimate
	if l.WeeklyReset.IsZero() {
		wk = rolling("weekly", Week, l.Weekly, s, l, now)
	} else {
		wk = anchored("weekly", Week, l.WeeklyReset, l.Weekly, s, l, now)
	}
	return LimitEstimate{
		Now: now, FiveHour: five, Weekly: wk,
		State: max(five.State, wk.State),
		pause: l.PauseLaunches,
	}
}

// rolling counts samples in (now-length, now]. Usage next drops when the
// oldest of them ages out.
func rolling(name string, length time.Duration, c Cap, s []sample, l Limits, now time.Time) WindowEstimate {
	start := now.Add(-length)
	w := WindowEstimate{Name: name, Start: start}
	var oldest time.Time
	w.sum(s, l, now, func(t time.Time) bool {
		if !t.After(start) {
			return false
		}
		if oldest.IsZero() || t.Before(oldest) {
			oldest = t
		}
		return true
	})
	if !oldest.IsZero() {
		w.ResetsAt = oldest.Add(length)
		w.ResetIn = w.ResetsAt.Sub(now)
	}
	w.rate(c, l)
	return w
}

// anchored counts samples in [start, now] where start is the latest
// anchor + k*length not after now.
func anchored(name string, length time.Duration, anchor time.Time, c Cap, s []sample, l Limits, now time.Time) WindowEstimate {
	k := now.Sub(anchor) / length
	start := anchor.Add(k * length)
	if start.After(now) { // anchor in the future: Duration division truncates toward zero
		start = start.Add(-length)
	}
	w := WindowEstimate{Name: name, Start: start, ResetsAt: start.Add(length)}
	w.ResetIn = w.ResetsAt.Sub(now)
	w.sum(s, l, now, func(t time.Time) bool { return !t.Before(start) })
	w.rate(c, l)
	return w
}

func (w *WindowEstimate) sum(s []sample, l Limits, now time.Time, in func(time.Time) bool) {
	unpriced := map[string]bool{}
	for _, x := range s {
		if x.t.After(now) || !in(x.t) {
			continue
		}
		w.Tokens = w.Tokens.Add(x.tokens)
		w.Messages += x.messages
		p, ok := PriceFor(x.model, l.Prices)
		if !ok {
			unpriced[x.model] = true
		}
		w.USD += p.Cost(x.tokens)
	}
	for m := range unpriced {
		w.Unpriced = append(w.Unpriced, m)
	}
	sort.Strings(w.Unpriced)
}

func (w *WindowEstimate) rate(c Cap, l Limits) {
	w.Cap = c
	if c.unlimited() {
		w.Unlimited = true
		return
	}
	if c.Tokens > 0 {
		w.TokenPercent = float64(w.Tokens.Total()) / float64(c.Tokens)
	}
	if c.USD > 0 {
		w.USDPercent = w.USD / c.USD
	}
	w.Percent = max(w.TokenPercent, w.USDPercent)
	switch {
	case w.Percent >= 1:
		w.State = Over
	case w.Percent >= l.warnAt():
		w.State = Warn
	}
}
