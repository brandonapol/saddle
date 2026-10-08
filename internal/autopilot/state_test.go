package autopilot

import (
	"path/filepath"
	"testing"
	"time"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autopilot.json")
	st, err := Load(path)
	if err != nil || st.On || st.Label() != DefaultReadyLabel {
		t.Fatalf("missing file = %+v, %v; want off with the default label", st, err)
	}
	at := time.Date(2026, 10, 8, 7, 0, 0, 0, time.UTC)
	st = State{On: true, Stop: Stop{Until: at, UntilUsage: 0.9, MaxTasks: 3}, ReadyLabel: "ready",
		LastTick: at, LastDecision: "spawned t1 for #4", Summary: "x", Spawned: []Spawned{{Issue: 4, Task: "t1", At: at}}}
	if err := st.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !got.On || !got.Stop.Until.Equal(at) || got.Stop.UntilUsage != 0.9 || got.Stop.MaxTasks != 3 ||
		got.Label() != "ready" || got.LastDecision != "spawned t1 for #4" || len(got.Spawned) != 1 || got.Summary != "x" {
		t.Fatalf("round trip = %+v", got)
	}
}

func TestParseUntil(t *testing.T) {
	loc := time.FixedZone("x", 2*3600)
	now := time.Date(2026, 10, 8, 22, 30, 0, 0, loc)
	cases := map[string]time.Time{
		"07:00": time.Date(2026, 10, 9, 7, 0, 0, 0, loc),   // tomorrow
		"23:15": time.Date(2026, 10, 8, 23, 15, 0, 0, loc), // later today
		"2026-10-10T05:00:00+02:00": time.Date(2026, 10, 10, 5, 0, 0, 0, loc),
	}
	for in, want := range cases {
		got, err := ParseUntil(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("ParseUntil(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "7pm", "25:00"} {
		if _, err := ParseUntil(bad, now); err == nil {
			t.Errorf("ParseUntil(%q) accepted", bad)
		}
	}
}

func TestParseUsage(t *testing.T) {
	for in, want := range map[string]float64{"90%": 0.9, "90": 0.9, "0.5": 0.5, "100": 1} {
		got, err := ParseUsage(in)
		if err != nil || got != want {
			t.Errorf("ParseUsage(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "0", "-5", "150%", "x"} {
		if _, err := ParseUsage(bad); err == nil {
			t.Errorf("ParseUsage(%q) accepted", bad)
		}
	}
}
