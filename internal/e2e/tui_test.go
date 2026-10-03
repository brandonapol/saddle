//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

func TestHarnessTUIDriver(t *testing.T) {
	w := world(t, Options{})
	u := w.StartTUI(120, 40)
	u.WaitScreen("1 control", "2 plan", "3 merge", "AGENTS", "ORCHESTRATOR")
	if err := u.Fits(120); err != nil {
		t.Fatal(err)
	}
	u.Resize(36, 40)
	Eventually(t, "the TUI to lay out for 36 columns", func() error { return u.Fits(36) })
	if s := u.Screen(); strings.Contains(s, "AGENTS") {
		t.Fatalf("36 columns still shows the side-by-side layout:\n%s", s)
	}
	u.Type("hello from e2e")
	u.Keys("Enter")
	u.WaitScreen("fake orchestrator ack")
	u.Quit()
}
