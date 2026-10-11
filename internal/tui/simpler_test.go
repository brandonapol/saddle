package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/store"
)

// #402: /simpler re-explains the orchestrator's last message through the
// narrator, not the orchestrator, and threads the answer into the chat.
func TestSimplerAsksNarratorAboutLastMessage(t *testing.T) {
	m, f := newAskModel(t)
	m.focus = focusChat
	m.addChat(store.ChatAssistant, "Landed t183; Watcher.Tiers gates max_tier.\n\nTestMaxTierGate passes.")
	f.answer = "Some safety work for auto-merge landed. Nothing needs you."
	typeIn(m, "/simpler")
	_, c := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	feed(m, c)
	if len(f.qs) != 1 || !strings.Contains(f.qs[0].Text, "Watcher.Tiers gates max_tier") || !strings.Contains(f.qs[0].Text, "three sentences") {
		t.Fatalf("narrator questions = %+v", f.qs)
	}
	n := len(m.chat)
	if m.chat[n-2].text != askQ+"/simpler" || m.chat[n-1].text != askA+f.answer {
		t.Fatalf("chat tail = %+v", m.chat[n-2:])
	}
	for _, c := range m.chat {
		if strings.Contains(c.text, "orchestrator isn't running") {
			t.Fatal("/simpler went to the orchestrator")
		}
	}
}

// With no orchestrator message yet, /simpler explains the current state.
func TestSimplerWithoutMessageExplainsState(t *testing.T) {
	m, f := newAskModel(t)
	m.focus = focusChat
	m.chat = nil
	feed(m, m.simpler())
	if len(f.qs) != 1 || !strings.Contains(f.qs[0].Text, "going on right now") {
		t.Fatalf("narrator questions = %+v", f.qs)
	}
}
