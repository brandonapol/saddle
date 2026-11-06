package app

import (
	"errors"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
)

// noWindowTmux is tmux refusing new windows (a full disk, a tmux error).
type noWindowTmux struct {
	*fakeTmux
	fail bool
}

func (f *noWindowTmux) NewWindow(name, dir, cmd string) (string, error) {
	if f.fail {
		return "", errors.New("tmux new-window: disk quota exceeded")
	}
	return f.fakeTmux.NewWindow(name, dir, cmd)
}

// #781: a hung takeover whose relaunch fails leaves the task lost, not
// windowless: ResumeLost picks it up on the next tick instead of it sitting
// as running forever, holding a slot and its claims.
func TestFailedTakeoverRelaunchIsResumedNextTick(t *testing.T) {
	t.Parallel()
	a, ft := setup(t)
	tk, err := a.Spawn(SpawnReq{Title: "one", Claims: []string{"one/**"}})
	must(t, err)
	nt := &noWindowTmux{fakeTmux: ft, fail: true}
	a.Tmux = nt
	tk, err = a.Store.Task(tk.ID)
	must(t, err)
	must(t, a.takeOver(tk, &HungWorker{Task: tk.ID}, time.Now(), "20m"))

	got, _ := a.Store.Task(tk.ID)
	if !a.lost(got, false) {
		t.Fatalf("after a failed relaunch the task is not lost (window %q, status %s): ResumeLost would never see it", got.Window, got.Status)
	}
	nt.fail = false
	rs, err := a.ResumeLost(false)
	must(t, err)
	if len(rs) != 1 || rs[0].Task != tk.ID {
		t.Fatalf("resumed %+v, want %s", rs, tk.ID)
	}
	got, _ = a.Store.Task(tk.ID)
	if !a.ownWindow(got) || got.Status != store.Running {
		t.Fatalf("after resume: window %q status %s", got.Window, got.Status)
	}
}
