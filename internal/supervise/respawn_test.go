package supervise

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// An exiting child is restarted until Keep says it was stopped on purpose.
func TestRespawnRestartsUntilUnwanted(t *testing.T) {
	var runs atomic.Int64
	var exits atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		Respawn(context.Background(), RespawnOptions{
			MinBackoff: time.Millisecond, MaxBackoff: 2 * time.Millisecond,
			Keep:   func() bool { return runs.Load() < 3 },
			OnExit: func(error, time.Duration) { exits.Add(1) },
		}, func(context.Context) error { runs.Add(1); return errors.New("killed") })
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Respawn did not stop once unwanted")
	}
	if runs.Load() != 3 || exits.Load() != 2 {
		t.Fatalf("runs %d, exits %d", runs.Load(), exits.Load())
	}
}

// Cancelling the context (ctrl-c) stops the loop without a restart.
func TestRespawnStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var runs atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		Respawn(ctx, RespawnOptions{MinBackoff: time.Millisecond}, func(ctx context.Context) error {
			runs.Add(1)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	<-done
	if runs.Load() != 1 {
		t.Fatalf("runs = %d", runs.Load())
	}
}
