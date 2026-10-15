package tui

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestFirstFrameFiresOnceOnTheFirstLaidOutView: saddle up starts its
// deferred startup work on the first frame (#635). The hook waits for a
// laid-out frame, not the blank one before the terminal size is known, and
// fires once however often the TUI redraws.
func TestFirstFrameFiresOnceOnTheFirstLaidOutView(t *testing.T) {
	var calls atomic.Int32
	fired := make(chan struct{}, 4)
	m := newViewModel(0, 0)
	WithFirstFrame(func() { calls.Add(1); fired <- struct{}{} })(m)
	m.width = 0
	m.View()
	select {
	case <-fired:
		t.Fatal("the hook fired on a blank frame")
	default:
	}
	m.width, m.height = 100, 30
	for range 3 {
		m.View()
	}
	select {
	case <-fired:
	case <-time.After(10 * time.Second):
		t.Fatal("the hook never fired")
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("hook fired %d times, want 1", n)
	}
}
