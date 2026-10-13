package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/brandonapol/saddle/internal/tmux"
)

// screenFixture is a captured pane from testdata, as the plain capture the
// watchers read.
func screenFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".ansi"))
	if err != nil {
		t.Fatal(err)
	}
	return tmux.StripANSI(string(b))
}

// Real codex 0.154 and grok 1.0.50 panes (#488). Grok's approval screen
// draws its options as "1 (●) Yes…" with no menu cursor or codex wording.
func TestDetectPromptCapturedPanes(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]string{
		"codex_trust":    PromptAsk,
		"codex_approval": PromptAsk,
		"codex_working":  PromptNone,
		"codex_idle":     PromptNone,
		"grok_approval":  PromptAsk,
		"grok_working":   PromptNone,
		"grok_idle":      PromptNone,
	} {
		if got := DetectPrompt(screenFixture(t, name)); got != want {
			t.Errorf("DetectPrompt(%s) = %q, want %q", name, got, want)
		}
	}
}

// The working indicator in real panes (#488): codex's "esc to interrupt",
// grok's "Ctrl+c:cancel" footer. The approval screens show it too; a
// prompt is handled before the indicator is read.
func TestWorkingIndicatorCapturedPanes(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		"codex_working": true,
		"codex_idle":    false,
		"codex_trust":   false,
		"grok_working":  true,
		"grok_approval": true,
		"grok_idle":     false,
	} {
		if got := workingRe.MatchString(screenFixture(t, name)); got != want {
			t.Errorf("working(%s) = %v, want %v", name, got, want)
		}
	}
}
