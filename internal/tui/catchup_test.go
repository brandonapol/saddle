package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// catchUpModel is storeChatModel with a repo root, so the last look has
// somewhere to be saved.
func catchUpModel(t *testing.T) *model {
	t.Helper()
	m := storeChatModel(t)
	m.keys = newKeyMap()
	m.app.Root = t.TempDir()
	if err := os.MkdirAll(filepath.Join(m.app.Root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	return m
}

// alt+c puts the catch-up in the chat (#412).
func TestAltCShowsCatchUp(t *testing.T) {
	m := catchUpModel(t)
	m.app.Store.Event("t1", "landed", "")
	runKey(m, altKey('c'))
	if !hasChat(m, "Since ") || !hasChat(m, "t1") {
		t.Fatalf("alt+c showed no catch-up naming t1: %+v", m.chat)
	}
}

// A keystroke saves the last look, and the next session's catch-up starts
// there instead of CatchUpDefault ago.
func TestKeystrokeRecordsLastLook(t *testing.T) {
	m := catchUpModel(t)
	before := time.Now().Add(-time.Second)
	runKey(m, altKey('d'))
	got := m.app.LastLooked()
	if got.Before(before.Truncate(time.Second)) || got.After(time.Now()) {
		t.Fatalf("LastLooked() = %v after a keystroke at about %v", got, before)
	}

	// A new session: its first key starts the catch-up at the saved look.
	looked := time.Now().Add(-3 * time.Hour)
	if err := m.app.MarkLooked(looked); err != nil {
		t.Fatal(err)
	}
	m2 := catchUpModel(t)
	m2.app = m.app
	m2.looked(time.Now())
	if m2.looks.from.Unix() != looked.Unix() {
		t.Fatalf("catch-up starts at %v, want the last look %v", m2.looks.from, looked)
	}
}

// Typing does not move the catch-up's start; coming back after a gap does.
func TestCatchUpStartsAtLastKeyBeforeGap(t *testing.T) {
	m := catchUpModel(t)
	t0 := time.Now().Add(-time.Hour)
	m.looked(t0)
	start := m.looks.from
	m.looked(t0.Add(time.Second))
	if !m.looks.from.Equal(start) {
		t.Fatalf("a keystroke without a gap moved the start to %v", m.looks.from)
	}
	back := t0.Add(time.Second + awayGap + time.Minute)
	m.looked(back)
	if want := t0.Add(time.Second); !m.looks.from.Equal(want) {
		t.Fatalf("after a gap the start is %v, want %v", m.looks.from, want)
	}
}
