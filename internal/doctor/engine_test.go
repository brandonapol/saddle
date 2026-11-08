package doctor

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
)

type engineEnv struct {
	rep EngineReport
	err error
}

func (e engineEnv) Engine() (EngineReport, error) { return e.rep, e.err }

// #789: doctor says when each watcher last ticked, and warns on a dead
// engine that is meant to run or a watcher that panicked or went stale.
func TestEngineCheck(t *testing.T) {
	ago := time.Now().Add(-14 * time.Minute)
	sentinel := app.WatcherHealth{Name: "sentinel", LastTick: ago, Ago: "14m", Stale: true}
	for _, c := range []struct {
		name   string
		rep    EngineReport
		status Status
		want   string
	}{
		{"never ran", EngineReport{}, "", ""},
		{"down but wanted", EngineReport{Down: "the saddle engine is down but meant to run"}, Warn, "down but meant to run"},
		{"stale watcher", EngineReport{Owner: app.LockEngine, Watchers: []app.WatcherHealth{sentinel}}, Warn, "sentinel last ticked 14m ago"},
		{"healthy", EngineReport{Owner: app.LockUp, Watchers: []app.WatcherHealth{{Name: "ci", Ago: "12s"}}}, OK, "running (up); ci last ticked 12s ago"},
		{"stopped", EngineReport{Watchers: []app.WatcherHealth{{Name: "ci", Ago: "3h"}}}, OK, "not running; ci last ticked 3h ago"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := engineCheck(engineEnv{rep: c.rep})
			if r.Status != c.status || !strings.Contains(r.Detail, c.want) {
				t.Fatalf("got %+v", r)
			}
		})
	}
	if r := engineCheck(engineEnv{err: errors.New("locked")}); r.Status != Warn {
		t.Fatalf("read error: %+v", r)
	}
}

// #908: the fix names commands that exist. A stale watcher that never
// panicked is hung, not crashed, so it isn't sent to look for a panic; one
// that panicked is sent to saddle events, which shows the stack.
func TestEngineCheckFixNamesRealCommands(t *testing.T) {
	hung := app.WatcherHealth{Name: "remote", Ago: "14m", Stale: true}
	r := engineCheck(engineEnv{rep: EngineReport{Owner: app.LockEngine, Watchers: []app.WatcherHealth{hung}}})
	if strings.Contains(r.Fix, "saddle log") || strings.Contains(r.Fix, "watcher_panic") {
		t.Errorf("hung watcher fix points at panics: %q", r.Fix)
	}
	if !strings.Contains(r.Fix, "remote") || !strings.Contains(r.Fix, "no panic") {
		t.Errorf("hung watcher fix: %q", r.Fix)
	}
	crashed := app.WatcherHealth{Name: "ci", Ago: "5s", Restarts: 2, LastPanic: "boom"}
	r = engineCheck(engineEnv{rep: EngineReport{Owner: app.LockEngine, Watchers: []app.WatcherHealth{crashed}}})
	if !strings.Contains(r.Fix, "saddle events --kind watcher_panic") || strings.Contains(r.Fix, "saddle log") {
		t.Errorf("panicked watcher fix: %q", r.Fix)
	}
	if r := engineCheck(engineEnv{err: errors.New("locked")}); r.Status != Warn {
		t.Fatalf("read error: %+v", r)
	}
}
