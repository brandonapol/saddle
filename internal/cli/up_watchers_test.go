package cli

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/supervise"
)

// #789: a panic in one watcher's tick no longer kills the process and every
// loop with it: the others keep ticking and the panicking one restarts.
func TestStartWatchersSurvivesAWatcherPanic(t *testing.T) {
	g := supervise.New(supervise.Options{MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond})
	var sentinelTicks, ciRuns atomic.Int64
	ws := []watcher{
		{"sentinel", func(ctx context.Context) {
			for ctx.Err() == nil {
				sentinelTicks.Add(1)
				supervise.Tick(ctx)
				time.Sleep(time.Millisecond)
			}
		}},
		{"ci", func(ctx context.Context) {
			ciRuns.Add(1)
			var p *struct{ n int }
			_ = p.n // an odd forge response: nil dereference
		}},
	}
	stop := startWatchersWith(context.Background(), g, ws)
	deadline := time.Now().Add(10 * time.Second)
	for ciRuns.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	before := sentinelTicks.Load()
	for sentinelTicks.Load() < before+5 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	stop()
	if ciRuns.Load() < 3 {
		t.Fatalf("the panicking watcher ran %d times, want restarts", ciRuns.Load())
	}
	if sentinelTicks.Load() < before+5 {
		t.Fatal("the other watcher stopped ticking")
	}
	for _, b := range g.Beats() {
		if b.Name == "ci" && b.Restarts < 2 {
			t.Fatalf("ci beat = %+v", b)
		}
	}
}
