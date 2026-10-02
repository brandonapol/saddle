package app

import (
	"strings"
	"testing"
)

func TestLockOwnerTellsTUIFromEngine(t *testing.T) {
	a, _ := setup(t)
	if got := a.LockOwner(); got != "" {
		t.Fatalf("free lock: owner %q", got)
	}
	release, err := a.AcquireLock(LockEngine)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.LockOwner(); got != LockEngine {
		t.Fatalf("owner %q, want engine", got)
	}
	_, err = a.AcquireLock(LockUp)
	if err == nil || !strings.Contains(err.Error(), "Claude Code session is orchestrating") {
		t.Fatalf("saddle up while the engine runs: err %v", err)
	}
	release()
	if got := a.LockOwner(); got != "" {
		t.Fatalf("released lock: owner %q", got)
	}

	release, err = a.AcquireLock(LockUp)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if got := a.LockOwner(); got != LockUp {
		t.Fatalf("owner %q, want up", got)
	}
	_, err = a.AcquireLock(LockEngine)
	if err == nil || !strings.Contains(err.Error(), "saddle up is already running") {
		t.Fatalf("engine while saddle up runs: err %v", err)
	}
}

func TestEnsureOrchestratorCreatesTaskOnce(t *testing.T) {
	a, _ := setup(t)
	t0, err := a.EnsureOrchestrator()
	if err != nil {
		t.Fatal(err)
	}
	if t0.ID != OrchestratorID {
		t.Fatalf("task %q", t0.ID)
	}
	if _, err := a.EnsureOrchestrator(); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if _, sess, err := a.Orchestrator(); err != nil || sess != "" {
		t.Fatalf("Orchestrator after EnsureOrchestrator: sess %q err %v", sess, err)
	}
}

func TestPluginBriefWaitsInBackgroundAndKeepsRules(t *testing.T) {
	a, _ := setup(t)
	brief := a.PluginBrief()
	for _, want := range []string{
		"user's own Claude Code session", "saddle plugin engine", "saddle plugin wait", "run_in_background",
		"tmux attach -t " + a.Cfg.Session,
		"DISJOINT path claims", "which tests must exist", "`unstack`", "failing-first", "‼ ",
	} {
		if !strings.Contains(brief, want) {
			t.Errorf("plugin brief missing %q", want)
		}
	}
	for _, not := range []string{"sidebar", "TUI renders"} {
		if strings.Contains(brief, not) {
			t.Errorf("plugin brief mentions the TUI: %q", not)
		}
	}
	if tui := a.orchestratorBrief(); strings.Contains(tui, "saddle plugin wait") || !strings.Contains(tui, "Never poll or wait in a loop") {
		t.Error("TUI brief picked up the plugin's waiting rules")
	}
}
