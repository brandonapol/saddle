package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/orchrc"
)

// Under saddle up --rc the TUI has no headless process: worker events it
// raises become orchestrator notices for the session's wake path, and the
// chat box points at the session instead of sending anywhere (#409).
func TestRCEventsBecomeNoticesNotStdin(t *testing.T) {
	m := slashModel(t)
	m.app.Root = t.TempDir()
	m.useRC(orchrc.Session{Name: "saddle: repo", Attach: "/bin/sh"})
	m.pending = []string{"t3 (db) stopped without calling done. It may be asking something."}
	m.deliver()
	if len(m.pending) != 0 {
		t.Errorf("events still pending: %q", m.pending)
	}
	if n, err := m.app.Store.PendingActionNotices(app.OrchestratorID); err != nil || n != 1 {
		t.Errorf("action notices for t0 = %d (%v), want 1", n, err)
	}

	m.sendNow("hello?")
	if last := m.chat[len(m.chat)-1].text; last != rcTypeHere {
		t.Errorf("a typed message under --rc says %q", last)
	}
}

// The orchestrator's pane opens on its own once the screen has a size.
func TestRCPaneOpensOnStart(t *testing.T) {
	m := newTermModel(t)
	m.useRC(orchrc.Session{Name: "saddle: repo", Attach: "/bin/sh"})
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.term == nil || !m.termOpen {
		t.Fatal("the orchestrator's pane didn't open")
	}
	if !strings.Contains(rcIntro(orchrc.Session{Name: "saddle: repo", Resumed: true}), `"saddle: repo"`) {
		t.Error("the intro doesn't name the Remote Control session")
	}
}
