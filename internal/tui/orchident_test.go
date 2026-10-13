package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brandonapol/saddle/internal/agent"
)

// #479: the header names the orchestrator's adapter next to its model, so
// after saddle up grok (or codex, whose orchestrator is Claude Code) the
// owner sees what the chat runs on.
func TestHeaderNamesOrchestratorAdapter(t *testing.T) {
	for _, c := range []struct {
		kind, model, want string
	}{
		{agent.KindClaude, "opus", "orchestrator claude · opus"},
		{agent.KindGrok, "grok-4-fast", "orchestrator grok · grok-4-fast"},
		{agent.KindClaude, "", "orchestrator claude · default"},
	} {
		m := slashModel(t)
		m.launch = agent.Launch{Kind: c.kind, Model: c.model}
		m.tasks = nil // leave the header room for the orchestrator segment
		m.Update(tea.WindowSizeMsg{Width: 200, Height: 40})
		if h := m.viewHeader(); !strings.Contains(h, c.want) {
			t.Errorf("%s header = %q, want %q", c.kind, h, c.want)
		}
	}
}
