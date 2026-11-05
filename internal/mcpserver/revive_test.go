package mcpserver

import (
	"os/exec"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

// #789: an engine that died while meant to run comes back on the next tool
// call any agent makes.
func TestToolCallRevivesADeadEngine(t *testing.T) {
	a := perfSetup(t, 0)
	var started []string
	old := app.StartEngine
	app.StartEngine = func(root string) error { started = append(started, root); return nil }
	t.Cleanup(func() { app.StartEngine = old })

	callTool(t, a, "t0", "status", map[string]any{}, nil)
	if len(started) != 0 {
		t.Fatal("started an engine nobody wanted")
	}
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	if err := a.WantEngine(dead.Process.Pid); err != nil {
		t.Fatal(err)
	}
	callTool(t, a, "t0", "status", map[string]any{}, nil)
	if len(started) != 1 || started[0] != a.Root {
		t.Fatalf("one call should revive the engine once: %v", started)
	}
}
