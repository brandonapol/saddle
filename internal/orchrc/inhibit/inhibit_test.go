package inhibit

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func found(string) (string, error)   { return "/usr/bin/x", nil }
func missing(string) (string, error) { return "", errors.New("not found") }

func TestForPicksThePlatformInhibitor(t *testing.T) {
	if tl, ok := For("linux", found); !ok || tl.Name != "systemd-inhibit" {
		t.Errorf("linux: %+v %v", tl, ok)
	}
	if tl, ok := For("darwin", found); !ok || tl.Name != "caffeinate" {
		t.Errorf("darwin: %+v %v", tl, ok)
	}
	if _, ok := For("linux", missing); ok {
		t.Error("linux without systemd-inhibit has an inhibitor")
	}
	if _, ok := For("windows", found); ok {
		t.Error("windows has an inhibitor")
	}
}

// The inhibitor is held exactly while autopilot is on, and released when
// it turns off or the watch ends.
func TestWatchHoldsWhileOn(t *testing.T) {
	var on atomic.Bool
	var mu sync.Mutex
	held, starts := 0, 0
	start := func(Tool) (func(), error) {
		mu.Lock()
		defer mu.Unlock()
		held++
		starts++
		return func() { mu.Lock(); held--; mu.Unlock() }, nil
	}
	state := func() (int, int) { mu.Lock(); defer mu.Unlock(); return held, starts }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		Watch(ctx, Tool{Name: "x"}, func() (bool, error) { return on.Load(), nil }, start, time.Millisecond, func(string, ...any) {})
		close(done)
	}()
	wait := func(want int) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			if h, _ := state(); h == want {
				return
			}
		}
		h, _ := state()
		t.Fatalf("held = %d, want %d", h, want)
	}
	wait(0)
	on.Store(true)
	wait(1)
	time.Sleep(10 * time.Millisecond)
	if _, s := state(); s != 1 {
		t.Errorf("started %d times while on, want once", s)
	}
	on.Store(false)
	wait(0)
	on.Store(true)
	wait(1)
	cancel()
	<-done
	if h, _ := state(); h != 0 {
		t.Error("the inhibitor outlived the watch")
	}
}
