package tmux

import (
	"sync"
	"testing"
	"time"
)

type fakeDriver struct {
	Driver
	mu    sync.Mutex
	pane  string
	sent  []string
	alive bool
}

func (f *fakeDriver) Alive(string) bool { return f.alive }
func (f *fakeDriver) Capture(string, int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pane, nil
}
func (f *fakeDriver) SendText(_, t string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, t)
	return nil
}
func (f *fakeDriver) setPane(p string) { f.mu.Lock(); f.pane = p; f.mu.Unlock() }
func (f *fakeDriver) count() int       { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sent) }

// Timing paces retries quickly without writing the package defaults, which
// parallel tests read (#384).
func (f *fakeDriver) Timing() Timing {
	return Timing{RetryEvery: 5 * time.Millisecond, RetryFor: 100 * time.Millisecond,
		ConfirmEvery: 5 * time.Millisecond, ConfirmFor: 60 * time.Millisecond}
}

func waitFor(cond func() bool) bool {
	for i := 0; i < 100; i++ {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

const empty = "output\n╭──────╮\n│ >    │\n╰──────╯"
const draft = "output\n╭──────╮\n│ > half typed │\n╰──────╯"

func TestHasDraft(t *testing.T) {
	for pane, want := range map[string]bool{
		empty: false, draft: true, "": false, "$ ls": false,
		"❯ hello": true, "❯": false, "> Try \"fix a bug\"": false,
	} {
		if got := HasDraft(pane); got != want {
			t.Errorf("HasDraft(%q) = %v, want %v", pane, got, want)
		}
	}
}

func TestSendsWhenEmpty(t *testing.T) {
	f := &fakeDriver{pane: empty, alive: true}
	SendWhenIdle(f, "@1", "hi", nil)
	if f.count() != 1 {
		t.Fatalf("sent %d, want 1", f.count())
	}
}

func TestDefersThenSends(t *testing.T) {
	f := &fakeDriver{pane: draft, alive: true}
	SendWhenIdle(f, "@1", "hi", nil)
	time.Sleep(20 * time.Millisecond)
	if f.count() != 0 {
		t.Fatal("sent over a draft")
	}
	f.setPane(empty)
	if !waitFor(func() bool { return f.count() == 1 }) {
		t.Fatal("never sent after draft cleared")
	}
}

func TestGivesUp(t *testing.T) {
	f := &fakeDriver{pane: draft, alive: true}
	SendWhenIdle(f, "@1", "hi", nil)
	time.Sleep(200 * time.Millisecond)
	f.setPane(empty)
	time.Sleep(50 * time.Millisecond)
	if f.count() != 0 {
		t.Fatal("sent after giving up")
	}
}

func TestNoLongerNeeded(t *testing.T) {
	f := &fakeDriver{pane: draft, alive: true}
	var mu sync.Mutex
	need := true
	SendWhenIdle(f, "@1", "hi", func() bool { mu.Lock(); defer mu.Unlock(); return need })
	mu.Lock()
	need = false
	mu.Unlock()
	f.setPane(empty)
	time.Sleep(50 * time.Millisecond)
	if f.count() != 0 {
		t.Fatal("sent although no longer needed")
	}
}
