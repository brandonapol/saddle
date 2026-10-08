package tui

import (
	"fmt"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/store"
)

// countingTmux is a tmux driver that counts the tmux processes each call
// would start.
type countingTmux struct {
	execs   int
	windows map[string]bool
}

func (c *countingTmux) HasSession() bool                          { c.execs++; return true }
func (c *countingTmux) NewSession(_, _, _ string) (string, error) { c.execs++; return "", nil }
func (c *countingTmux) NewWindow(_, _, _ string) (string, error)  { c.execs++; return "", nil }
func (c *countingTmux) KillWindow(string) error                   { c.execs++; return nil }
func (c *countingTmux) Alive(id string) bool                      { c.execs++; return c.windows[id] }
func (c *countingTmux) SendText(_, _ string) error                { c.execs++; return nil }
func (c *countingTmux) SendKeys(string, ...string) error          { c.execs++; return nil }
func (c *countingTmux) KillSession() error                        { c.execs++; return nil }
func (c *countingTmux) Capture(id string, _ int) (string, error) {
	c.execs++
	return "screen of " + id, nil
}
func (c *countingTmux) Windows() (map[string]bool, error) { c.execs++; return c.windows, nil }
func (c *countingTmux) CaptureMany(m map[string]int) (map[string]string, error) {
	c.execs++
	out := map[string]string{}
	for id := range m {
		out[id] = "screen of " + id
	}
	return out, nil
}

func idleAgents(n int) ([]mcpserver.TaskView, *countingTmux) {
	tm := &countingTmux{windows: map[string]bool{}}
	var ts []mcpserver.TaskView
	for i := 1; i <= n; i++ {
		w := fmt.Sprintf("@%d", i)
		tm.windows[w] = true
		ts = append(ts, mcpserver.TaskView{ID: fmt.Sprintf("t%d", i), Status: store.Idle, Window: w})
	}
	return ts, tm
}

// With 16 idle agents a refresh starts at most two tmux processes, and the
// selected agent's peek still updates every tick (#272).
func TestScreensTmuxExecsPerTick(t *testing.T) {
	ts, tm := idleAgents(16)
	a := &app.App{Tmux: tm}
	for tick := range 9 {
		tm.execs = 0
		sweep := tick%sweepEvery == 0
		got := captureScreens(a, ts, "t5", true, sweep)
		if tm.execs > 2 {
			t.Fatalf("tick %d: %d tmux execs, want at most 2", tick, tm.execs)
		}
		if !got.peekOK || got.peek != "screen of @5" {
			t.Fatalf("tick %d: selected peek = %+v", tick, got)
		}
		if sweep && len(got.screens) != 16 {
			t.Fatalf("tick %d: sweep captured %d screens, want 16", tick, len(got.screens))
		}
		if !sweep && len(got.screens) != 1 {
			t.Fatalf("tick %d: off-sweep captured %d screens, want only the selected", tick, len(got.screens))
		}
	}
}

// A hidden peek isn't captured between sweeps, and a dead window is not
// captured at all.
func TestScreensHiddenPeekAndDeadWindow(t *testing.T) {
	ts, tm := idleAgents(3)
	a := &app.App{Tmux: tm}
	if got := captureScreens(a, ts, "t1", false, false); got.peekOK || tm.execs > 1 {
		t.Fatalf("hidden peek off-sweep: %+v after %d execs", got, tm.execs)
	}
	delete(tm.windows, "@1")
	got := captureScreens(a, ts, "t1", true, true)
	if !got.peekOK || got.peek != "" || len(got.screens) != 2 {
		t.Fatalf("dead selected window: %+v", got)
	}
}
