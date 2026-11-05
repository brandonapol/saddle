package app

import (
	"os"

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
