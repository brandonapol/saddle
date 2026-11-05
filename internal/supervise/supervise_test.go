package supervise

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type rec struct {
	mu     sync.Mutex
	events []string
	ticks  map[string]int
}

func (r *rec) Event(kind, data string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, kind+": "+data)
}

func (r *rec) Tick(b Beat) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ticks == nil {
		r.ticks = map[string]int{}
	}
	r.ticks[b.Name]++
}

func (r *rec) count(kind string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if strings.HasPrefix(e, kind+":") {
			n++
		}
	}
	return n
}

func fast(r Recorder) Options {
	return Options{MinBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond, Heartbeat: time.Millisecond, PersistEvery: time.Nanosecond, Recorder: r}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(2 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("timed out waiting for " + what)
}

// A panic in one watcher's tick must not stop the others, and the panicking
// watcher must restart (#789).
func TestPanickingWatcherRestartsOthersKeepTicking(t *testing.T) {
	r := &rec{}
	g := New(fast(r))
	ctx, cancel := context.WithCancel(context.Background())
	var good, badRuns, badTicks atomic.Int64
	g.Go(ctx, "good", func(ctx context.Context) {
		for ctx.Err() == nil {
			good.Add(1)
			Tick(ctx)
			time.Sleep(time.Millisecond)
		}
	})
	g.Go(ctx, "bad", func(ctx context.Context) {
		badRuns.Add(1)
		for i := 0; ; i++ {
			badTicks.Add(1)
			Tick(ctx)
			if i == 2 {
				panic("boom: nil map") // as a nil map write in a real loop would
			}
			time.Sleep(time.Millisecond)
		}
	})
	waitFor(t, "bad to restart twice", func() bool { return badRuns.Load() >= 3 })
	before := good.Load()
	waitFor(t, "good to keep ticking", func() bool { return good.Load() > before+5 })
	cancel()
	g.Wait()

	if n := r.count(EventPanic); n < 2 {
		t.Fatalf("want watcher_panic events, got %d: %v", n, r.events)
	}
	if !strings.Contains(r.events[0], "bad panicked") || !strings.Contains(r.events[0], "goroutine") {
		t.Fatalf("panic event should name the watcher and carry the stack: %q", r.events[0])
	}
	if r.count(EventRestart) < 2 {
		t.Fatalf("want watcher_restart events: %v", r.events)
	}
	beats := map[string]Beat{}
	for _, b := range g.Beats() {
		beats[b.Name] = b
	}
	if beats["bad"].Restarts < 2 || !strings.Contains(beats["bad"].LastPanic, "boom") {
		t.Fatalf("bad beat = %+v", beats["bad"])
	}
	if beats["good"].Restarts != 0 || beats["good"].At.IsZero() {
		t.Fatalf("good beat = %+v", beats["good"])
	}
	if r.ticks["good"] == 0 || r.ticks["bad"] == 0 {
		t.Fatalf("heartbeats not persisted: %v", r.ticks)
	}
}

// A watcher that returns on its own is done: it is not restarted.
func TestReturnedWatcherIsNotRestarted(t *testing.T) {
	g := New(fast(nil))
	var runs atomic.Int64
	g.Go(context.Background(), "once", func(context.Context) { runs.Add(1) })
	g.Wait()
	if runs.Load() != 1 {
		t.Fatalf("runs = %d", runs.Load())
	}
}

// The supervisor ticks a running watcher's heartbeat even when the loop
// never calls Tick.
func TestSupervisorTicksRunningWatcher(t *testing.T) {
	g := New(fast(nil))
	ctx, cancel := context.WithCancel(context.Background())
	g.Go(ctx, "quiet", func(ctx context.Context) { <-ctx.Done() })
	var first time.Time
	waitFor(t, "first beat", func() bool {
		b := g.Beats()
		if len(b) == 1 {
			first = b[0].At
		}
		return !first.IsZero()
	})
	waitFor(t, "a later beat", func() bool { return g.Beats()[0].At.After(first) })
	cancel()
	g.Wait()
}

func TestTickOutsideSupervisorIsNoop(t *testing.T) {
	Tick(context.Background())
}
