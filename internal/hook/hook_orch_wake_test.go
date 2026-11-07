package hook

import (
	"testing"
	"time"
)

// #860: every orchestrator hook call is a heartbeat for the hook wake path,
// so the engine's wake alarm can tell a working session from an idle one.
func TestOrchestratorHookBeatsTheHookPath(t *testing.T) {
	a := setup(t)
	if _, err := a.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	if a.HookActive(time.Now()) {
		t.Fatal("hook path active before any hook call")
	}
	HandleOrchestrator(a, Input{Event: "PostToolUse"})
	if !a.HookActive(time.Now()) {
		t.Fatal("hook path absent right after a hook call")
	}
	if a.HookActive(time.Now().Add(20 * time.Minute)) {
		t.Fatal("hook path still active after the session went quiet")
	}
}
