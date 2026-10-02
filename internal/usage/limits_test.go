package usage

import (
	"math"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func rec(t time.Time, model string, in, out int64) Record {
	return Record{Time: t, Model: model, Tokens: Tokens{Input: in, Output: out}}
}

func TestPriceForLongestPrefixAndDateSuffix(t *testing.T) {
	cases := []struct {
		model string
		in    float64
	}{
		{"claude-opus-5-5", 4},
		{"claude-opus-5", 5},
		{"claude-sonnet-5-5", 2},
		{"claude-sonnet-4-6", 3},
		{"claude-haiku-4-5-20251001", 1},
		{"claude-fable-5-1", 10},
	}
	for _, c := range cases {
		p, ok := PriceFor(c.model, nil)
		if !ok || p.Input != c.in {
			t.Errorf("PriceFor(%q) = %+v, %v; want input %v", c.model, p, ok, c.in)
		}
	}
	if _, ok := PriceFor("gpt-5", nil); ok {
		t.Error("unknown model should not be priced")
	}
	// Overrides win, including over a longer built-in prefix.
	p, ok := PriceFor("claude-opus-5-5", map[string]Price{"claude-opus": {Input: 99}})
	if !ok || p.Input != 99 {
		t.Errorf("override = %+v, %v", p, ok)
	}
}

func TestCostMath(t *testing.T) {
	p := Price{Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5}
	tok := Tokens{Input: 1_000_000, Output: 500_000, CacheRead: 2_000_000, CacheCreation: 100_000}
	// 4 + 10 + 0.4 + 0.5
	if got := p.Cost(tok); !near(got, 14.9) {
		t.Errorf("cost = %v, want 14.9", got)
	}
	// Opus 5.5 built-in: 1M in + 1M out = 4 + 20.
	if got := CostUSD("claude-opus-5-5", Tokens{Input: 1e6, Output: 1e6}, nil); !near(got, 24) {
		t.Errorf("opus 5.5 = %v", got)
	}
	if got := CostUSD("mystery", Tokens{Input: 1e6}, nil); got != 0 {
		t.Errorf("unknown model cost = %v", got)
	}
}

func TestFiveHourRollingWindowBoundaries(t *testing.T) {
	now := ts("2026-01-01T15:00:00Z")
	m := "claude-haiku-4-5"
	recs := []Record{
		rec(now.Add(-5*time.Hour), m, 1000, 0),             // exactly 5h old: aged out
		rec(now.Add(-5*time.Hour+time.Second), m, 100, 0),  // oldest inside
		rec(now.Add(-1*time.Hour), m, 10, 0),               // inside
		rec(now, m, 1, 0),                                  // at now: inside
		rec(now.Add(time.Minute), m, 1_000_000, 1_000_000), // future: ignored
	}
	e := Estimate(recs, Limits{FiveHour: Cap{Tokens: 1000}}, now)
	w := e.FiveHour
	if w.Tokens.Input != 111 || w.Messages != 3 {
		t.Fatalf("five-hour used = %+v msgs=%d", w.Tokens, w.Messages)
	}
	if !near(w.Percent, 0.111) {
		t.Errorf("percent = %v", w.Percent)
	}
	// Rolling: usage first drops when the oldest in-window record ages out.
	if want := now.Add(time.Second); !w.ResetsAt.Equal(want) || w.ResetIn != time.Second {
		t.Errorf("resets at %v in %v, want %v", w.ResetsAt, w.ResetIn, want)
	}
	if !w.Start.Equal(now.Add(-5 * time.Hour)) {
		t.Errorf("start = %v", w.Start)
	}
}

func TestEmptyRollingWindowHasNoReset(t *testing.T) {
	now := ts("2026-01-01T15:00:00Z")
	e := Estimate(nil, Limits{FiveHour: Cap{USD: 10}}, now)
	if !e.FiveHour.ResetsAt.IsZero() || e.FiveHour.ResetIn != 0 || e.FiveHour.Percent != 0 {
		t.Errorf("empty window = %+v", e.FiveHour)
	}
	if e.State != OK {
		t.Errorf("state = %v", e.State)
	}
}

func TestWeeklyAnchoredReset(t *testing.T) {
	// Anchor: a Monday 09:00 UTC, weeks before now.
	anchor := ts("2025-12-01T09:00:00Z")
	now := ts("2026-01-07T12:00:00Z") // Wednesday; current week began Mon 2026-01-05 09:00
	m := "claude-haiku-4-5"
	recs := []Record{
		rec(ts("2026-01-05T08:59:59Z"), m, 1000, 0), // previous week
		rec(ts("2026-01-05T09:00:00Z"), m, 7, 0),    // first instant of this week
		rec(ts("2026-01-07T11:00:00Z"), m, 3, 0),
	}
	e := Estimate(recs, Limits{Weekly: Cap{Tokens: 100}, WeeklyReset: anchor}, now)
	w := e.Weekly
	if w.Tokens.Input != 10 {
		t.Fatalf("weekly used = %+v", w.Tokens)
	}
	if want := ts("2026-01-05T09:00:00Z"); !w.Start.Equal(want) {
		t.Errorf("start = %v, want %v", w.Start, want)
	}
	if want := ts("2026-01-12T09:00:00Z"); !w.ResetsAt.Equal(want) || w.ResetIn != want.Sub(now) {
		t.Errorf("resets at %v in %v", w.ResetsAt, w.ResetIn)
	}
	// Anchored reset is fixed even with no usage.
	e = Estimate(nil, Limits{WeeklyReset: anchor}, now)
	if want := ts("2026-01-12T09:00:00Z"); !e.Weekly.ResetsAt.Equal(want) {
		t.Errorf("empty anchored resets at %v", e.Weekly.ResetsAt)
	}
	// An anchor in the future still yields the enclosing week.
	e = Estimate(nil, Limits{WeeklyReset: ts("2026-03-02T09:00:00Z")}, now)
	if want := ts("2026-01-12T09:00:00Z"); !e.Weekly.ResetsAt.Equal(want) {
		t.Errorf("future anchor resets at %v", e.Weekly.ResetsAt)
	}
}

func TestWeeklyRollingWithoutAnchor(t *testing.T) {
	now := ts("2026-01-08T00:00:00Z")
	m := "claude-haiku-4-5"
	recs := []Record{
		rec(ts("2026-01-01T00:00:00Z"), m, 50, 0), // exactly 7d: out
		rec(ts("2026-01-02T00:00:00Z"), m, 5, 0),
	}
	e := Estimate(recs, Limits{}, now)
	if e.Weekly.Tokens.Input != 5 {
		t.Errorf("weekly rolling used = %+v", e.Weekly.Tokens)
	}
	if want := ts("2026-01-09T00:00:00Z"); !e.Weekly.ResetsAt.Equal(want) {
		t.Errorf("resets at %v", e.Weekly.ResetsAt)
	}
}

func TestZeroCapsMeanUnlimited(t *testing.T) {
	now := ts("2026-01-01T15:00:00Z")
	recs := []Record{rec(now, "claude-opus-5-5", 1e9, 1e9)}
	e := Estimate(recs, Limits{PauseLaunches: true}, now)
	for _, w := range []WindowEstimate{e.FiveHour, e.Weekly} {
		if !w.Unlimited || w.Percent != 0 || w.State != OK {
			t.Errorf("%s = %+v", w.Name, w)
		}
		if w.USD == 0 {
			t.Errorf("%s: usage still reported when unlimited, got $0", w.Name)
		}
	}
	if e.State != OK || e.ShouldPauseLaunches() {
		t.Errorf("state = %v pause = %v", e.State, e.ShouldPauseLaunches())
	}
}

func TestPercentIsWorstOfTokenAndUSDCaps(t *testing.T) {
	now := ts("2026-01-01T15:00:00Z")
	// 1M input on Opus 5.5 = $4.
	recs := []Record{rec(now, "claude-opus-5-5", 1_000_000, 0)}
	e := Estimate(recs, Limits{FiveHour: Cap{Tokens: 10_000_000, USD: 5}}, now)
	w := e.FiveHour
	if !near(w.USD, 4) || !near(w.TokenPercent, 0.1) || !near(w.USDPercent, 0.8) || !near(w.Percent, 0.8) {
		t.Errorf("window = %+v", w)
	}
}

func TestThresholdTransitions(t *testing.T) {
	now := ts("2026-01-01T15:00:00Z")
	m := "claude-haiku-4-5"
	cases := []struct {
		used   int64
		warnAt float64
		want   State
	}{
		{0, 0, OK},
		{79, 0, OK},      // default warn 0.8
		{80, 0, Warn},    // exactly at warn
		{99, 0, Warn},    // just under cap
		{100, 0, Over},   // at cap
		{150, 0, Over},   // past cap
		{49, 0.5, OK},    // custom warn
		{50, 0.5, Warn},  // custom warn
		{95, 0.95, Warn}, // custom warn
	}
	for _, c := range cases {
		recs := []Record{rec(now, m, c.used, 0)}
		e := Estimate(recs, Limits{FiveHour: Cap{Tokens: 100}, WarnAt: c.warnAt}, now)
		if e.FiveHour.State != c.want || e.State != c.want {
			t.Errorf("used=%d warnAt=%v: window %v overall %v, want %v", c.used, c.warnAt, e.FiveHour.State, e.State, c.want)
		}
	}
}

func TestOverallStateIsWorstWindowAndPause(t *testing.T) {
	now := ts("2026-01-01T15:00:00Z")
	m := "claude-haiku-4-5"
	// Old usage counts weekly but not in the 5h window.
	recs := []Record{rec(now.Add(-24*time.Hour), m, 100, 0), rec(now, m, 10, 0)}
	l := Limits{FiveHour: Cap{Tokens: 100}, Weekly: Cap{Tokens: 100}}
	e := Estimate(recs, l, now)
	if e.FiveHour.State != OK || e.Weekly.State != Over || e.State != Over {
		t.Fatalf("states 5h=%v wk=%v all=%v", e.FiveHour.State, e.Weekly.State, e.State)
	}
	if e.ShouldPauseLaunches() {
		t.Error("pause must be opt-in")
	}
	l.PauseLaunches = true
	if !Estimate(recs, l, now).ShouldPauseLaunches() {
		t.Error("over with PauseLaunches should pause")
	}
	// Warn never pauses.
	l.Weekly.Tokens = 130
	if e := Estimate(recs, l, now); e.State != Warn || e.ShouldPauseLaunches() {
		t.Errorf("warn: state=%v pause=%v", e.State, e.ShouldPauseLaunches())
	}
}

func TestUnpricedModelsReported(t *testing.T) {
	now := ts("2026-01-01T15:00:00Z")
	recs := []Record{rec(now, "gpt-5", 10, 0), rec(now, "gpt-5", 10, 0), rec(now, "claude-haiku-4-5", 10, 0)}
	e := Estimate(recs, Limits{}, now)
	if len(e.FiveHour.Unpriced) != 1 || e.FiveHour.Unpriced[0] != "gpt-5" {
		t.Errorf("unpriced = %v", e.FiveHour.Unpriced)
	}
}

func TestEstimateBucketsUsesMinute(t *testing.T) {
	now := ts("2026-01-01T15:00:00Z")
	b := []Bucket{
		{Key: Key{Minute: now.Add(-5 * time.Hour), Model: "claude-haiku-4-5"}, Tokens: Tokens{Input: 9}, Messages: 2},
		{Key: Key{Minute: now.Add(-time.Hour), Task: "t1", Model: "claude-haiku-4-5"}, Tokens: Tokens{Input: 1_000_000}, Messages: 3},
	}
	e := EstimateBuckets(b, Limits{}, now)
	if e.FiveHour.Tokens.Input != 1_000_000 || e.FiveHour.Messages != 3 || !near(e.FiveHour.USD, 1) {
		t.Errorf("five-hour = %+v", e.FiveHour)
	}
	if e.Weekly.Messages != 5 {
		t.Errorf("weekly msgs = %d", e.Weekly.Messages)
	}
}

func TestStateString(t *testing.T) {
	if OK.String() != "ok" || Warn.String() != "warn" || Over.String() != "over" {
		t.Error("state strings")
	}
}
