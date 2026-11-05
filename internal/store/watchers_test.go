package store

import (
	"testing"
	"time"
)

func TestWatcherTicksRoundTrip(t *testing.T) {
	s := openTest(t)
	if ts, err := s.WatcherTicks(); err != nil || len(ts) != 0 {
		t.Fatalf("fresh store: %v %v", ts, err)
	}
	at := time.Now().Add(-14 * time.Minute)
	if err := s.TickWatcher(WatcherTick{Name: "sentinel", At: at, PID: 7}); err != nil {
		t.Fatal(err)
	}
	if err := s.TickWatcher(WatcherTick{Name: "sentinel", At: at.Add(time.Minute), PID: 8, Restarts: 2, LastPanic: "boom"}); err != nil {
		t.Fatal(err)
	}
	ts, err := s.WatcherTicks()
	if err != nil || len(ts) != 1 {
		t.Fatalf("ticks = %v %v", ts, err)
	}
	if got := ts[0]; !got.At.Equal(at.Add(time.Minute)) || got.PID != 8 || got.Restarts != 2 || got.LastPanic != "boom" {
		t.Fatalf("tick = %+v", got)
	}
}
