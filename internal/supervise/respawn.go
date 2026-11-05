package supervise

import (
	"context"
	"time"
)

// RespawnOptions tune Respawn. Zero values pick the defaults.
type RespawnOptions struct {
	// MinBackoff is the wait before restarting a child that exited; it
	// doubles while children keep dying young, up to MaxBackoff, and resets
	// once one has run for StableAfter.
	MinBackoff, MaxBackoff, StableAfter time.Duration
	// Keep reports whether the child is still meant to run; checked after
	// each exit and before each restart. Nil means always.
	Keep func() bool
	// OnExit hears each exit that will be followed by a restart.
	OnExit func(err error, wait time.Duration)
}

// Respawn runs run until ctx ends or Keep says stop, restarting it with
// backoff each time it returns: the loop behind `saddle plugin engine
// --supervise` (#789). run must return once ctx ends.
func Respawn(ctx context.Context, o RespawnOptions, run func(ctx context.Context) error) {
	if o.MinBackoff <= 0 {
		o.MinBackoff = time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = time.Minute
	}
	if o.StableAfter <= 0 {
		o.StableAfter = 5 * time.Minute
	}
	keep := func() bool { return ctx.Err() == nil && (o.Keep == nil || o.Keep()) }
	backoff := o.MinBackoff
	for keep() {
		start := time.Now()
		err := run(ctx)
		if !keep() {
			return
		}
		if time.Since(start) >= o.StableAfter {
			backoff = o.MinBackoff
		}
		if o.OnExit != nil {
			o.OnExit(err, backoff)
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(2*backoff, o.MaxBackoff)
	}
}
