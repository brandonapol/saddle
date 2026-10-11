package cli

import (
	"strings"
	"testing"
)

// screenTmux is a tmux whose every window is alive and shows screen.
type screenTmux struct {
	fakeTmux
	screen string
	lines  int
}

func (s *screenTmux) Alive(string) bool { return true }
func (s *screenTmux) Capture(_ string, n int) (string, error) {
	s.lines = n
	return s.screen, nil
}

// The mod's needs-you band (#168) peeks through `saddle peek`: it prints the
// last lines of a task's terminal, as the MCP peek tool returns them.
func TestPeekPrintsTheTasksScreen(t *testing.T) {
	a := queueRepo(t)
	tm := &screenTmux{screen: "Do you want to proceed?\n❯ 1. Yes\n  2. No\n\n"}
	a.Tmux = tm
	var out strings.Builder
	if err := peek(&out, a, "t1", 12); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "Do you want to proceed?\n❯ 1. Yes\n  2. No\n"; got != want {
		t.Fatalf("peek printed %q, want %q", got, want)
	}
	if tm.lines != 12 {
		t.Fatalf("captured %d lines, want 12", tm.lines)
	}
}

func TestPeekRefusesATaskWithoutAWindow(t *testing.T) {
	a := queueRepo(t)
	a.Tmux = &fakeTmux{}
	var out strings.Builder
	err := peek(&out, a, "t1", 0)
	if err == nil || !strings.Contains(err.Error(), "no live window") {
		t.Fatalf("peek = %v, want no live window", err)
	}
}
