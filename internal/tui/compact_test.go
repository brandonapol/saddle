package tui

import "testing"

type fakeOrch struct {
	busy bool
	sent []string
}

func (f *fakeOrch) Busy() bool { return f.busy }
func (f *fakeOrch) Send(s string) error {
	f.sent = append(f.sent, s)
	return nil
}

// #179: while the orchestrator process runs, the compact watcher sees its
// busy state, whether the owner is typing, and can send to it; once it
// exits the target is gone.
func TestCompactTargetFollowsOrchestrator(t *testing.T) {
	m := newViewModel(120, 40)
	p := &fakeOrch{}
	t.Cleanup(func() { m.app.SetCompactTarget(nil) })
	m.registerCompact(p)
	tg := m.app.CompactTarget()
	if tg == nil {
		t.Fatal("no compact target registered for the running orchestrator")
	}
	if tg.Busy() || tg.Drafting() {
		t.Fatal("idle orchestrator with an empty input reads busy or drafting")
	}
	p.busy = true
	if !tg.Busy() {
		t.Fatal("Busy doesn't follow the process")
	}
	m.Update(runeKey('h'))
	if !tg.Drafting() {
		t.Fatal("text in the input box must read as drafting")
	}
	if err := tg.Send("/compact"); err != nil || len(p.sent) != 1 {
		t.Fatalf("send: %v %q", err, p.sent)
	}
	m.Update(closedMsg{}) // the process's event stream ended
	if m.app.CompactTarget() != nil {
		t.Fatal("target still registered after the orchestrator exited")
	}
}
