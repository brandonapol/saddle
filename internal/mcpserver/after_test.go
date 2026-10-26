package mcpserver

import (
	"slices"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

// #675: the after tool records edges for the caller's own task by default;
// a worker can't record them for another task.
func TestAfterTool(t *testing.T) {
	a, _ := stackSetup(t)
	for _, id := range []string{"w1", "w2", "w3"} {
		if err := a.Store.CreateTask(store.Task{ID: id, Title: id, Role: store.RoleWorker}); err != nil {
			t.Fatal(err)
		}
	}
	var out OK
	if res := callTool(t, a, "w2", "after", map[string]any{"after": []string{"w1"}}, &out); res.IsError {
		t.Fatalf("after failed: %s", errText(res))
	}
	if got := a.TaskAfter("w2"); !slices.Equal(got, []string{"w1"}) {
		t.Fatalf("after = %v, want [w1]", got)
	}
	if res := callTool(t, a, "w2", "after", map[string]any{"task": "w3", "after": []string{"w1"}}, nil); !res.IsError {
		t.Fatal("a worker recorded an edge for another task")
	}
	if res := callTool(t, a, app.OrchestratorID, "after", map[string]any{"task": "w3", "after": []string{"w1"}}, nil); res.IsError {
		t.Fatalf("orchestrator: %s", errText(res))
	}
	if got := a.TaskAfter("w3"); !slices.Equal(got, []string{"w1"}) {
		t.Fatalf("w3 after = %v, want [w1]", got)
	}
}
