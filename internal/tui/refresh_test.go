package tui

import (
	"errors"
	"testing"
	"time"
)

// A refresh slower than the tick must not let refreshes pile up and land out
// of order. Requests made while one runs collapse into a single follow-up.
func TestRefreshRunsOneAtATime(t *testing.T) {
	m := &model{}
	if m.refresh() == nil {
		t.Fatal("first refresh did not start")
	}
	for range 3 {
		if m.refresh() != nil {
			t.Fatal("a second refresh started while the first was running")
		}
		m.Update(tickMsg(time.Now()))
	}
	if !m.stale {
		t.Fatal("requests made during a refresh were dropped")
	}

	m.Update(refreshMsg{err: errors.New("db locked")})
	if !m.refreshing || m.stale {
		t.Fatalf("after the first refresh landed: refreshing=%v stale=%v, want one follow-up running", m.refreshing, m.stale)
	}
	if m.flash == "" {
		t.Fatal("a failed refresh was not shown")
	}

	m.Update(refreshMsg{err: errors.New("db locked")})
	if m.refreshing || m.stale {
		t.Fatalf("after the follow-up landed: refreshing=%v stale=%v, want idle", m.refreshing, m.stale)
	}
	if m.refresh() == nil {
		t.Fatal("refresh did not start once idle")
	}
}
