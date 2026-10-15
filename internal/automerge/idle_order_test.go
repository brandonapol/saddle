package automerge

import (
	"testing"
	"time"
)

// #613: a busy tick logged its idle event inside the state update, before it
// saved the file status reads, so a reader reacting to the event saw the
// previous check. The file must hold the busy tick when the event fires.
func TestIdleEventFiresAfterStateSaved(t *testing.T) {
	r := newRig(t, "", "t3")
	c := &clock{time.Date(2026, 10, 3, 12, 41, 0, 0, time.Local)}
	r.w.Now = c.now
	r.on(t)
	r.w.Lock = func() (func(), bool, error) { return nil, false, nil }

	var seen []State
	r.w.Event = func(task, kind, data string) {
		if kind != EventIdle {
			return
		}
		s, err := Load(r.w.Path)
		if err != nil {
			t.Fatal(err)
		}
		seen = append(seen, s)
	}
	check(t, r.w)
	if len(seen) != 1 {
		t.Fatalf("idle events = %d, want 1", len(seen))
	}
	s := seen[0]
	if !s.Last.Busy || s.Last.Checked.IsZero() || s.Idle == "" {
		t.Fatalf("state at idle event is stale: busy=%v checked=%v idle=%q", s.Last.Busy, s.Last.Checked, s.Idle)
	}
}
