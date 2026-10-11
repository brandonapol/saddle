package tmux

import (
	"sync"
	"testing"
	"time"
)

// pacedDriver is a fakeDriver that sets its own delivery timing, the way a
// parallel test does instead of writing the package defaults (#384).
type pacedDriver struct {
	*fakeDriver
	timing Timing
}

func (p pacedDriver) Timing() Timing { return p.timing }

// #384: a driver's own timing is used instead of the package defaults, so a
// parallel test can speed delivery up without racing other tests.
func TestDeliverUsesDriverTiming(t *testing.T) {
	t.Parallel()
	f := pacedDriver{&fakeDriver{pane: draft, alive: true}, Timing{
		RetryEvery: 5 * time.Millisecond, RetryFor: 10 * time.Second,
		ConfirmEvery: 5 * time.Millisecond, ConfirmFor: 10 * time.Second,
	}}
	var mu sync.Mutex
	sent := false
	Deliver(f, "@1", "hi", Delivery{Sent: func() { mu.Lock(); sent = true; mu.Unlock() }})
	f.setPane(empty)
	// The defaults retry every 3s and confirm every 250ms; well inside that
	// the driver's pace must have typed the text and seen the pane change.
	deadline := time.Now().Add(200 * time.Millisecond)
	for f.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if f.count() != 1 {
		t.Fatalf("typed %d times within 200ms, want 1: driver timing ignored", f.count())
	}
	f.setPane("changed")
	for time.Now().Before(deadline.Add(200 * time.Millisecond)) {
		mu.Lock()
		ok := sent
		mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("Sent not called at the driver's confirm pace")
}

// #384: a confirm goroutine that wakes after its deadline, as one does on a
// loaded machine, still looks at the pane once before it gives up.
func TestConfirmChecksAtLeastOnce(t *testing.T) {
	t.Parallel()
	f := pacedDriver{&fakeDriver{pane: "before", alive: true}, Timing{
		RetryEvery: time.Millisecond, RetryFor: time.Millisecond,
		ConfirmEvery: 20 * time.Millisecond, ConfirmFor: time.Nanosecond,
	}}
	done := make(chan struct{})
	go func() {
		confirm(f, "@1", "different", func() { close(done) })
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("confirm gave up without looking at the pane")
	}
}
