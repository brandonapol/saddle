//go:build e2e

package e2e

import (
	"os"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/e2e/fakeagent"
)

// orchLogHas waits for the fake orchestrator's log to contain want.
func orchLogHas(t *testing.T, w *World, want string) {
	t.Helper()
	Eventually(t, "the orchestrator log to show "+want, func() error {
		b, _ := os.ReadFile(fakeagent.OrchestratorLog(w.Scripts))
		if !strings.Contains(string(b), want) {
			return errorf("orchestrator log:\n%s", b)
		}
		return nil
	})
}

// #265: Esc in the chat, or one ctrl+c, interrupts a turn stuck in a tool
// call. The chat says Interrupted, the same session takes the next message,
// and saddle up keeps running.
func TestJourneyInterruptOrchestrator(t *testing.T) {
	w := world(t, Options{})
	u := w.StartTUI(140, 40)
	u.WaitScreen("ORCHESTRATOR")

	u.Type("slow: build the thing")
	u.Keys("Enter")
	u.WaitScreen("Bash sleep 600", "esc to interrupt")
	u.Keys("Escape")
	orchLogHas(t, w, "control: interrupt")
	u.WaitScreen("Interrupted.")
	u.WaitGone("esc to interrupt")

	u.Type("hello again")
	u.Keys("Enter")
	u.WaitScreen("fake orchestrator ack: hello again")

	// ctrl+c once interrupts and arms quitting; it doesn't quit.
	u.Type("slow: second")
	u.Keys("Enter")
	u.WaitScreen("esc to interrupt")
	u.Keys("C-c")
	u.WaitScreen("Ctrl+C again to quit")
	Eventually(t, "two interrupts", func() error {
		b, _ := os.ReadFile(fakeagent.OrchestratorLog(w.Scripts))
		if n := strings.Count(string(b), "control: interrupt"); n != 2 {
			return errorf("%d interrupts in:\n%s", n, b)
		}
		return nil
	})
	u.WaitGone("esc to interrupt")
	u.Quit()
}
