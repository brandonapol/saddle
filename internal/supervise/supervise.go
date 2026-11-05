// Package supervise keeps saddle's background loops alive (#789). Each
// watcher runs under Go, which recovers a panic, reports it with its stack,
// and restarts the watcher after a backoff, so one bad forge response or
// state file no longer takes every loop down with the process. Each watcher
// also has a heartbeat: the supervisor ticks it while the watcher runs, and a
// loop may tick it itself with Tick, so status and doctor can say when each
// one last ticked.
package supervise

import (
	"context"
	"fmt"
	"runtime/debug"
	"sort"
	"sync"
	"time"
)

// Event kinds a Recorder receives.
const (
	EventPanic   = "watcher_panic"
	EventRestart = "watcher_restart"
)

// Recorder persists what the supervisor sees. Both methods must be safe for
// concurrent use; errors are the recorder's to swallow.
type Recorder interface {
	// Event records a panic (kind EventPanic, data holds the stack) or a
	// restart.
	Event(kind, data string)
	// Tick persists watcher's heartbeat.
	Tick(Beat)
}

// Beat is one watcher's heartbeat.
type Beat struct {
	Name      string
	At        time.Time // last tick
	Restarts  int       // panics recovered so far
	LastPanic string    // the last panic's value, "" when none
}

// Options tune a Group. Zero values pick the defaults.
type Options struct {
	// MinBackoff is the wait before the first restart; it doubles on each
	// panic up to MaxBackoff, and resets once a run lasts StableAfter.
	MinBackoff, MaxBackoff, StableAfter time.Duration
	// Heartbeat is how often the supervisor ticks a running watcher.
	Heartbeat time.Duration
	// PersistEvery throttles Recorder.Tick per watcher.
	PersistEvery time.Duration
	Recorder     Recorder
}

func (o Options) withDefaults() Options {
	if o.MinBackoff <= 0 {
		o.MinBackoff = time.Second
	}
	if o.MaxBackoff <= 0 {
		o.MaxBackoff = time.Minute
	}
	if o.StableAfter <= 0 {
		o.StableAfter = 5 * time.Minute
	}
	if o.Heartbeat <= 0 {
		o.Heartbeat = 30 * time.Second
	}
	if o.PersistEvery <= 0 {
		o.PersistEvery = 30 * time.Second
	}
	return o
}

// Group supervises a set of watchers and holds their heartbeats in memory.
type Group struct {
	opts  Options
	wg    sync.WaitGroup
	mu    sync.Mutex
	beats map[string]*beat
}

type beat struct {
	Beat
	persisted time.Time
}

// New returns an empty Group.
func New(o Options) *Group {
	return &Group{opts: o.withDefaults(), beats: map[string]*beat{}}
}

// Go runs fn under supervision in its own goroutine until ctx ends: a panic
// is recovered, recorded and fn restarted after a backoff. fn returning
// normally ends its supervision; a watcher that has nothing to do may stop.
func (g *Group) Go(ctx context.Context, name string, fn func(ctx context.Context)) {
	g.wg.Go(func() { g.run(ctx, name, fn) })
}

// Wait blocks until every watcher has returned.
func (g *Group) Wait() { g.wg.Wait() }

func (g *Group) run(ctx context.Context, name string, fn func(ctx context.Context)) {
	backoff := g.opts.MinBackoff
	for ctx.Err() == nil {
		start := time.Now()
		panicked := g.once(ctx, name, fn)
		if !panicked {
			return
		}
		if time.Since(start) >= g.opts.StableAfter {
			backoff = g.opts.MinBackoff
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		backoff = min(2*backoff, g.opts.MaxBackoff)
		if g.opts.Recorder != nil {
			g.opts.Recorder.Event(EventRestart, name+" restarted after a panic")
		}
	}
}

// once runs fn one time, ticking its heartbeat while it runs, and reports
// whether it panicked.
func (g *Group) once(ctx context.Context, name string, fn func(ctx context.Context)) (panicked bool) {
	runCtx, cancel := context.WithCancel(context.WithValue(ctx, tickKey{}, func() { g.tick(name) }))
	defer cancel()
	g.tick(name)
	go func() {
		t := time.NewTicker(g.opts.Heartbeat)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				g.tick(name)
			}
		}
	}()
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			g.panicked(name, r, debug.Stack())
		}
	}()
	fn(runCtx)
	return false
}

func (g *Group) panicked(name string, r any, stack []byte) {
	g.mu.Lock()
	b := g.get(name)
	b.Restarts++
	b.LastPanic = fmt.Sprint(r)
	g.mu.Unlock()
	if g.opts.Recorder != nil {
		g.opts.Recorder.Event(EventPanic, fmt.Sprintf("%s panicked: %v\n%s", name, r, stack))
	}
}

func (g *Group) get(name string) *beat {
	b := g.beats[name]
	if b == nil {
		b = &beat{Beat: Beat{Name: name}}
		g.beats[name] = b
	}
	return b
}

func (g *Group) tick(name string) {
	now := time.Now()
	g.mu.Lock()
	b := g.get(name)
	b.At = now
	persist := g.opts.Recorder != nil && now.Sub(b.persisted) >= g.opts.PersistEvery
	if persist {
		b.persisted = now
	}
	snap := b.Beat
	g.mu.Unlock()
	if persist {
		g.opts.Recorder.Tick(snap)
	}
}

// Beats returns every watcher's heartbeat, by name.
func (g *Group) Beats() []Beat {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Beat, 0, len(g.beats))
	for _, b := range g.beats {
		out = append(out, b.Beat)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type tickKey struct{}

// Tick records a heartbeat for the watcher whose context ctx is. A loop
// calls it once per cycle so its heartbeat means "did work", not just "is
// running". Outside a supervised watcher it does nothing.
func Tick(ctx context.Context) {
	if f, ok := ctx.Value(tickKey{}).(func()); ok {
		f()
	}
}
