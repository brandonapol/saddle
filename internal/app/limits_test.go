package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/usage"
)

func addRecentUsage(t *testing.T, a *App, tokens int64) {
	t.Helper()
	b := usage.Bucket{
		Key:      usage.Key{Minute: time.Now().Add(-10 * time.Minute), Task: "t9", Model: "claude-opus-5-5"},
		Tokens:   usage.Tokens{Input: tokens},
		Messages: 1,
	}
	must(t, a.Store.AddUsage("s1", false, []usage.Bucket{b}))
}

func TestLimitsEstimateFromStore(t *testing.T) {
	t.Parallel()
	a, _ := meterApp(t)
	a.Cfg.Limits = usage.Limits{FiveHour: usage.Cap{Tokens: 1000}}
	addRecentUsage(t, a, 900)
	e, err := a.Limits(time.Now())
	must(t, err)
	if e.FiveHour.Tokens.Total() != 900 || e.FiveHour.State != usage.Warn || e.Weekly.Tokens.Total() != 900 || !e.Weekly.Unlimited {
		t.Fatalf("estimate: %+v", e)
	}
}

// Over a cap with pause_launches on, spawn refuses with an error that tells
// the orchestrator why and how to get past it; force still spawns.
func TestSpawnRefusedWhenOverCap(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.Cfg.Limits = usage.Limits{FiveHour: usage.Cap{Tokens: 1000}, PauseLaunches: true}
	addRecentUsage(t, a, 1500)

	_, err := a.Spawn(SpawnReq{Title: "meter"})
	if !errors.Is(err, ErrLaunchesPaused) {
		t.Fatalf("spawn over cap: err = %v", err)
	}
	for _, want := range []string{"5h", "150%", "pause_launches", "force"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if ts, _ := a.Store.Tasks(); len(ts) != 0 {
		t.Fatalf("refused spawn left tasks: %+v", ts)
	}
	if _, err := a.Spawn(SpawnReq{Title: "meter", Force: true}); err != nil {
		t.Fatalf("forced spawn: %v", err)
	}
}

// Without pause_launches, being over a cap only shows in the usage strip.
func TestSpawnAllowedOverCapWithoutPause(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.Cfg.Limits = usage.Limits{FiveHour: usage.Cap{Tokens: 1000}}
	addRecentUsage(t, a, 1500)
	if _, err := a.Spawn(SpawnReq{Title: "meter"}); err != nil {
		t.Fatal(err)
	}
}
