package app

import (
	"fmt"
	"os"
	"time"

	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/supervise"
)

// Events the watcher supervisor records (#789).
const (
	EventWatcherPanic   = supervise.EventPanic
	EventWatcherRestart = supervise.EventRestart
)

// WatcherRecorder is where the supervisor of saddle's background loops
// records panics, restarts and heartbeats: the event log and the
// watcher_ticks table, which status and doctor read.
func (a *App) WatcherRecorder() supervise.Recorder { return watcherRecorder{a} }

type watcherRecorder struct{ a *App }

func (r watcherRecorder) Event(kind, data string) { r.a.Store.Event("", kind, data) }

func (r watcherRecorder) Tick(b supervise.Beat) {
	_ = r.a.Store.TickWatcher(store.WatcherTick{Name: b.Name, At: b.At, PID: os.Getpid(), Restarts: b.Restarts, LastPanic: b.LastPanic})
}

// WatcherStaleAfter is how long a running engine's watcher may go without a
// heartbeat before status and doctor call it stale. The supervisor ticks
// each running watcher every 30s.
const WatcherStaleAfter = 5 * time.Minute

// WatcherHealth is one background watcher as status and doctor show it.
type WatcherHealth struct {
	Name      string    `json:"name"`
	LastTick  time.Time `json:"last_tick"`
	Ago       string    `json:"ago" jsonschema:"how long ago it last ticked, e.g. 14m"`
	Restarts  int       `json:"restarts,omitempty" jsonschema:"panics recovered since the engine started"`
	LastPanic string    `json:"last_panic,omitempty"`
	// Stale is set when the engine runs but this watcher has not ticked for
	// WatcherStaleAfter: it is wedged or gone.
	Stale bool `json:"stale,omitempty"`
}

// Line is the watcher in words: "sentinel last ticked 14m ago".
func (w WatcherHealth) Line() string {
	s := w.Name + " last ticked " + w.Ago + " ago"
	if w.Restarts > 0 {
		s += fmt.Sprintf(", restarted %d× after a panic (last: %s)", w.Restarts, w.LastPanic)
	}
	if w.Stale {
		s += ", stale"
	}
	return s
}

// Watchers is every background watcher's last heartbeat. Stale is only set
// while an engine (saddle up or the plugin engine) runs: with none running,
// every heartbeat is old and EngineDown says why.
func (a *App) Watchers(now time.Time) ([]WatcherHealth, error) {
	ts, err := a.Store.WatcherTicks()
	if err != nil {
		return nil, err
	}
	running := a.LockOwner() != ""
	out := make([]WatcherHealth, 0, len(ts))
	for _, t := range ts {
		out = append(out, WatcherHealth{
			Name: t.Name, LastTick: t.At, Ago: agoWords(now.Sub(t.At)),
			Restarts: t.Restarts, LastPanic: t.LastPanic,
			Stale: running && now.Sub(t.At) > WatcherStaleAfter,
		})
	}
	return out, nil
}

// EngineDown reports why nothing is watching agents, "" when an engine runs
// or none is meant to.
func (a *App) EngineDown() string {
	if a.LockOwner() != "" {
		return ""
	}
	if _, ok := a.EngineWanted(); ok {
		return "the saddle engine is down but meant to run; the next saddle call from any agent starts it again (or run `saddle plugin engine --supervise`)"
	}
	return ""
}

func agoWords(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
