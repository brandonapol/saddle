package app

import (
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/usage"
)

// #796: a limit banner with no reset backs off per adapter: the second one
// right after the first expires keeps it out at least 2h, not another 1h.
func TestNoResetBannerBacksOffExponentially(t *testing.T) {
	t.Parallel()
	a, _, _ := rotationApp(t)
	w, err := a.Spawn(SpawnReq{Title: "grok work", Adapter: usage.Grok, Force: true})
	must(t, err)

	must(t, a.AdapterLimitHit(w.ID, usage.LimitBanner{}))
	first := a.ExhaustedAdapters(time.Now())[usage.Grok]
	if d := time.Until(first.Until); d < 55*time.Minute || d > 65*time.Minute {
		t.Fatalf("first no-reset banner: out for %s, want 1h", d)
	}

	// The hour passes: grok is restored, picked again and hits the limit at once.
	must(t, a.ClearExhausted(usage.Grok))
	must(t, a.AdapterLimitHit(w.ID, usage.LimitBanner{}))
	second := a.ExhaustedAdapters(time.Now())[usage.Grok]
	if d := time.Until(second.Until); d < 2*time.Hour-time.Minute {
		t.Fatalf("second no-reset banner right after a restore: out for %s, want at least 2h", d)
	}

	// A successful run resets the backoff.
	a.limitWorked(usage.Grok)
	must(t, a.ClearExhausted(usage.Grok))
	must(t, a.AdapterLimitHit(w.ID, usage.LimitBanner{}))
	if d := time.Until(a.ExhaustedAdapters(time.Now())[usage.Grok].Until); d > 65*time.Minute {
		t.Fatalf("after a successful run: out for %s, want 1h again", d)
	}
}

func TestLimitBackoffCapsAtADay(t *testing.T) {
	for n, want := range map[int]time.Duration{0: time.Hour, 1: 2 * time.Hour, 2: 4 * time.Hour, 5: 24 * time.Hour, 40: 24 * time.Hour} {
		if got := limitBackoff(n); got != want {
			t.Errorf("limitBackoff(%d) = %s, want %s", n, got, want)
		}
	}
}

// #796: an out-of-credits banner never resets on a timer: no Until, so the
// adapter stays out until ClearExhausted.
func TestCreditsBannerStaysOutUntilCleared(t *testing.T) {
	t.Parallel()
	a, _, _ := rotationApp(t)
	b, ok := usage.DetectLimitBanner("Error: You are out of credits. Add credits to continue.")
	if !ok || !b.Credits {
		t.Fatalf("credits banner = %+v %v, want Credits", b, ok)
	}
	w, err := a.Spawn(SpawnReq{Title: "grok work", Adapter: usage.Grok, Force: true})
	must(t, err)
	must(t, a.AdapterLimitHit(w.ID, b))
	e, out := a.ExhaustedAdapters(time.Now())[usage.Grok]
	if !out || !e.Until.IsZero() {
		t.Fatalf("credits banner: out=%v until=%v, want out with no Until", out, e.Until)
	}
	if _, still := a.ExhaustedAdapters(time.Now().Add(100 * 24 * time.Hour))[usage.Grok]; !still {
		t.Fatal("credits exhaustion expired on a timer")
	}
	if !strings.Contains(a.RotationBanner(time.Now()), "grok until cleared") {
		t.Errorf("banner = %q", a.RotationBanner(time.Now()))
	}
	must(t, a.ClearExhausted(usage.Grok))
	if _, still := a.ExhaustedAdapters(time.Now())[usage.Grok]; still {
		t.Fatal("ClearExhausted did not restore it")
	}
}

// #796: a task on an adapter out of credits is handed off whatever
// handoff_wait says: there is no reset to wait for.
func TestCreditsExhaustionHandsOffDespiteHandoffWait(t *testing.T) {
	t.Parallel()
	a, ft, _ := rotationApp(t)
	a.Cfg.AdapterPolicy = config.AdapterPolicy{Failover: config.FailoverHandoff, HandoffWait: time.Hour}
	w, err := a.Spawn(SpawnReq{Title: "grok work", Adapter: usage.Grok, Force: true})
	must(t, err)
	_ = ft
	to, err := a.LimitHit(w.ID, usage.LimitBanner{Credits: true})
	must(t, err)
	if to == "" || to == usage.Grok {
		t.Fatalf("handed to %q, want another adapter", to)
	}
}
