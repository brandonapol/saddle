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

// waitFor polls cond until it holds or 10s pass. The budget is generous so
// a loaded runner doesn't fail a wait that would succeed; a passing run
// returns as soon as cond does.
func waitFor(cond func() bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}

// attempts counts Deliver's attempts through its Needed hook, which runs
// before each one, so a test can wait for retries instead of sleeping.
type attempts struct {
	mu   sync.Mutex
	n    int
	need bool
}

func newAttempts() *attempts { return &attempts{need: true} }

func (a *attempts) needed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.n++
	return a.need
}

func (a *attempts) count() int { a.mu.Lock(); defer a.mu.Unlock(); return a.n }

// waitAttempts waits until Deliver has made at least n attempts.
func (a *attempts) waitAttempts(t *testing.T, n int) {
	t.Helper()
	if !waitFor(func() bool { return a.count() >= n }) {
		t.Fatalf("Deliver made %d attempts, want %d", a.count(), n)
	}
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
	a := newAttempts()
	SendWhenIdle(f, "@1", "hi", a.needed)
	a.waitAttempts(t, 2) // a retry looked at the draft and held off
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
	a := newAttempts()
	SendWhenIdle(f, "@1", "hi", a.needed)
	// The retry window opens when the retry goroutine starts, which a
	// loaded runner can delay; wait for its first attempt.
	a.waitAttempts(t, 2)
	// Giving up is the absence of further attempts, so there is no event to
	// wait on: sleep past RetryFor (100ms, see fakeDriver.Timing) measured
	// from that first retry, then clear the draft.
	time.Sleep(200 * time.Millisecond)
	f.setPane(empty)
	gaveUpAt := a.count()
	// Long enough for several retries at RetryEvery (5ms) had it not given
	// up; again nothing to wait on but time.
	time.Sleep(50 * time.Millisecond)
	if f.count() != 0 || a.count() != gaveUpAt {
		t.Fatalf("sent %d, attempts %d -> %d after giving up", f.count(), gaveUpAt, a.count())
	}
}

func TestNoLongerNeeded(t *testing.T) {
	f := &fakeDriver{pane: draft, alive: true}
	a := newAttempts()
	SendWhenIdle(f, "@1", "hi", a.needed)
	a.mu.Lock()
	a.need = false
	before := a.n
	a.mu.Unlock()
	f.setPane(empty)
	// The next attempt reads Needed, sees false and ends the delivery.
	a.waitAttempts(t, before+1)
	if f.count() != 0 {
		t.Fatal("sent although no longer needed")
	}
}
