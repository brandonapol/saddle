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
