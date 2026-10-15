package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/brandonapol/saddle/internal/store"
)

// An issue-backed spawn is new work by definition: a ready ticket whose
// claims cover a file a landed task changed must start, not hit the #340
// confirm guard on every tick (#604).
func TestAutopilotSpawnsIssueOverLandedFile(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	landTask(t, a, "t1", "first", map[string]string{"a.go": "package a\n\nfunc A() int { return 1 }\n"})

	// The orchestrator's spawns with no issue keep the guard.
	if _, err := a.Spawn(SpawnReq{Title: "repair", Claims: []string{"a.go"}}); !errors.Is(err, ErrNeedsConfirm) {
		t.Fatalf("orchestrator spawn with no issue: err = %v, want ErrNeedsConfirm", err)
	}

	env := &autopilotEnv{a: a}
	id, err := env.Spawn(autopilot.SpawnReq{Title: "second", Issue: 7, Claims: []string{"a.go"}})
	if err != nil {
		t.Fatalf("issue-backed spawn over landed a.go: %v", err)
	}
	tk, err := a.Store.Task(id)
	must(t, err)
	if tk.Issue != 7 || tk.Status != store.Running {
		t.Fatalf("task = %+v, want issue 7 running", tk)
	}
}

// A confirm refusal autopilot can't answer reaches the orchestrator once as an
// action notice, not as a silent autopilot_skip every tick (#604, #119).
func TestAutopilotSurfacesNeedsConfirmOnce(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	landTask(t, a, "t1", "first", map[string]string{"a.go": "package a\n\nfunc A() int { return 1 }\n"})

	_, err := a.Store.TakeNotices(OrchestratorID, false) // t1's done notice
	must(t, err)

	env := &autopilotEnv{a: a}
	req := autopilot.SpawnReq{Title: "redo a", Claims: []string{"a.go"}}
	if _, err := env.Spawn(req); !errors.Is(err, ErrNeedsConfirm) {
		t.Fatalf("err = %v, want ErrNeedsConfirm", err)
	}
	ns, err := a.Store.TakeNotices(OrchestratorID, true)
	must(t, err)
	if len(ns) != 1 || !strings.Contains(ns[0].Text, "redo a") || !strings.Contains(ns[0].Text, "confirm") {
		t.Fatalf("notices = %+v, want one action notice naming the ticket", ns)
	}

	// The next tick refuses again, but the orchestrator already knows.
	if _, err := env.Spawn(req); !errors.Is(err, ErrNeedsConfirm) {
		t.Fatalf("second tick: err = %v, want ErrNeedsConfirm", err)
	}
	if ns, _ := a.Store.TakeNotices(OrchestratorID, true); len(ns) != 0 {
		t.Fatalf("second tick notices = %+v, want none", ns)
	}
}
