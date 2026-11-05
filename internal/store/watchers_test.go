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

// Every watcher ticks at once when the engine starts: each tick lands, even
// the ones racing to create the table.
func TestWatcherTicksConcurrentFirstWrites(t *testing.T) {
	s := openTest(t)
	errs := make(chan error, 12)
	for i := range 12 {
		go func() { errs <- s.TickWatcher(WatcherTick{Name: string(rune('a' + i)), At: time.Now()}) }()
	}
	for range 12 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if ts, err := s.WatcherTicks(); err != nil || len(ts) != 12 {
		t.Fatalf("ticks = %d, %v", len(ts), err)
	}
}
