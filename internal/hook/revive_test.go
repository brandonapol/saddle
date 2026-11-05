package hook

import (
	"os/exec"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

// #789: any agent's hook revives an engine that died while meant to run.
func TestHookRevivesADeadEngine(t *testing.T) {
	a := setup(t)
	n := 0
	old := app.StartEngine
	app.StartEngine = func(string) error { n++; return nil }
	t.Cleanup(func() { app.StartEngine = old })
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	if err := a.WantEngine(dead.Process.Pid); err != nil {
		t.Fatal(err)
	}
	run(t, a, "t1", map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Read", "tool_input": map[string]any{}})
	if n != 1 {
		t.Fatalf("engine starts = %d, want 1", n)
	}
}
