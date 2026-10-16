package mcpserver

import (
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

// #625: the keep tool records the owner's "do not kill" for the hung-worker
// watch, and refuses a task that doesn't exist.
func TestKeepTool(t *testing.T) {
	a, _ := stackSetup(t)
	tk, err := a.Spawn(app.SpawnReq{Title: "long migration"})
	if err != nil {
		t.Fatal(err)
	}
	if res := callTool(t, a, app.OrchestratorID, "keep", map[string]any{"task": tk.ID}, nil); res.IsError {
		t.Fatalf("keep: %s", errText(res))
	}
	es, err := a.Store.Events(50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range es {
		found = found || (e.Kind == app.EventHungKeep && e.Task == tk.ID)
	}
	if !found {
		t.Fatal("no hung_keep event")
	}
	if res := callTool(t, a, app.OrchestratorID, "keep", map[string]any{"task": "t999"}, nil); !res.IsError {
		t.Fatal("kept a task that doesn't exist")
	}
}
