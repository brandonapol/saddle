package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

// #404 acceptance: a bare "2" answers the oldest open decision card. Saddle
// confirms in one line and tells the orchestrator to run that option.
func TestBareNumberAnswersOldestDecisionCard(t *testing.T) {
	m := slashModel(t)
	m.app.Root = t.TempDir()
	out := fakeProc(t, m)
	old := app.RiskyPromptCard("t7", "t7", "Do you want to run rm -rf build?")
	old.Opened = time.Now().Add(-time.Hour)
	young := app.RiskyPromptCard("t8", "t8", "Do you want to push?")
	must(t, m.app.OpenDecision(young))
	must(t, m.app.OpenDecision(old))

	m.input.SetValue("2")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})

	if !hasChat(m, "✓ ") || !hasChat(m, "option 2") {
		t.Fatalf("no one-line confirmation: %+v", m.chat)
	}
	got := sentUser(t, out, 1)
	if len(got) != 1 || !strings.Contains(got[0], "send_keys t7 Escape") {
		t.Fatalf("orchestrator got %q, want t7's option 2", got)
	}
	open, err := m.app.OpenDecisions()
	must(t, err)
	if len(open) != 1 || open[0].Task != "t8" {
		t.Fatalf("open = %+v, want only t8's card", open)
	}
}

// With no open card, a bare number is an ordinary message.
func TestBareNumberWithoutCardIsAMessage(t *testing.T) {
	m := slashModel(t)
	m.app.Root = t.TempDir()
	out := fakeProc(t, m)
	m.input.SetValue("2")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := sentUser(t, out, 1); len(got) != 1 || got[0] != "2" {
		t.Fatalf("orchestrator got %q", got)
	}
}

// A risky prompt opens a decision card for the owner.
func TestRiskyTriageOpensDecisionCard(t *testing.T) {
	m := slashModel(t)
	m.app.Root = t.TempDir()
	must(t, m.app.Store.CreateTask(store.Task{ID: "t7", Title: "cleanup", Role: store.RoleWorker, Status: store.Running}))
	m.openRiskyCard(attention{task: "t7", title: "cleanup", screen: "Bash: rm -rf build\nDo you want to proceed?\n❯ 1. Yes"})
	open, err := m.app.OpenDecisions()
	must(t, err)
	if len(open) != 1 || open[0].Kind != app.DecisionRiskyPrompt || !strings.Contains(open[0].What, "Do you want to proceed?") {
		t.Fatalf("open = %+v", open)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
