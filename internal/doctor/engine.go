package doctor

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

// CheckEngine reports whether the engine that runs saddle's background
// watchers is up, and when each watcher last ticked (#789).
const CheckEngine = "engine"

const engineAbout = "the process that runs saddle's background watchers (sentinel, CI, auto-merge, resume...), and when each last ticked"

// EngineReport is what the engine check reads.
type EngineReport struct {
	// Owner holds the engine lock: up, up-rc, engine, or "" for nobody.
	Owner string
	// Down is set when the engine is meant to run but isn't (app.EngineDown).
	Down     string
	Watchers []app.WatcherHealth
}

// EngineEnv is an Env that can read the engine's state. The check is
// skipped for envs without it.
type EngineEnv interface {
	Engine() (EngineReport, error)
}

func engineCheck(ee EngineEnv) Result {
	rep, err := ee.Engine()
	if err != nil {
		return warn(CheckEngine, "could not read: "+err.Error(), "")
	}
	if rep.Down != "" {
		return warn(CheckEngine, rep.Down, "run `saddle plugin engine --supervise` in the background, or saddle up")
	}
	if rep.Owner == "" && len(rep.Watchers) == 0 {
		return Result{} // never ran here: nothing to say
	}
	var lines, panicked, hung []string
	for _, w := range rep.Watchers {
		lines = append(lines, w.Line())
		switch {
		case w.Restarts > 0:
			panicked = append(panicked, w.Name)
		case w.Stale:
			hung = append(hung, w.Name)
		}
	}
	detail := "not running"
	if rep.Owner != "" {
		detail = "running (" + rep.Owner + ")"
	}
	if len(lines) > 0 {
		detail += "; " + strings.Join(lines, "; ")
	}
	if rep.Owner != "" && (len(panicked) > 0 || len(hung) > 0) {
		return warn(CheckEngine, detail, engineFix(rep.Owner, panicked, hung))
	}
	return ok(CheckEngine, detail)
}

// Engine reads the engine lock, the keep-alive marker and the watchers'
// heartbeats.
func (s system) Engine() (EngineReport, error) {
	a := &app.App{Root: string(s)}
	rep := EngineReport{Owner: a.LockOwner(), Down: a.EngineDown()}
	db := filepath.Join(string(s), ".saddle", "state.db")
	if !s.Exists(db) {
		return rep, nil
	}
	st, err := store.Open(db)
	if err != nil {
		return rep, err
	}
	defer func() { _ = st.Close() }()
	a.Store = st
	rep.Watchers, err = a.Watchers(time.Now())
	return rep, err
}

// engineFix says how to look into, and clear, watchers that panicked or
// went stale. It names only commands that exist (#908).
func engineFix(owner string, panicked, hung []string) string {
	var parts []string
	if len(panicked) > 0 {
		parts = append(parts, "`saddle events --kind watcher_panic` shows the stack of each panic in "+strings.Join(panicked, ", "))
	}
	if len(hung) > 0 {
		parts = append(parts, strings.Join(hung, ", ")+" stopped ticking with no panic recorded, so it is hung, not crashed (`saddle events -n 50` shows what happened last)")
	}
	restart := "restart the engine to start every watcher fresh: `saddle plugin engine --stop`, then `saddle plugin engine --supervise` in the background"
	if owner == app.LockUp {
		restart = "restart the engine to start every watcher fresh: quit saddle up and run it again"
	}
	return strings.Join(parts, "; ") + "; " + restart
}
