package mcpserver

import (
	"context"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// #119.5: the orchestrator has the escape hatches as tools, so it never
// edits state.db: unstack, sentinel_ack and requeue.
func TestEscapeHatchTools(t *testing.T) {
	a, _ := stackSetup(t)
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := New(a, app.OrchestratorID).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	call := func(name string, args map[string]any) *mcp.CallToolResult {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	if res := call("sentinel_ack", nil); !res.IsError {
		t.Fatal("sentinel_ack with no flag succeeded")
	}
	if err := a.SetFlag(app.StackFlag{Task: "t1", Cause: "x"}); err != nil {
		t.Fatal(err)
	}
	if res := call("sentinel_ack", nil); res.IsError {
		t.Fatalf("sentinel_ack: %+v", res.Content)
	}
	if res := call("requeue", map[string]any{"task": "t1"}); !res.IsError {
		t.Fatal("requeue of work integration has succeeded")
	}
	if res := call("unstack", map[string]any{"task": "t1"}); res.IsError {
		t.Fatalf("unstack: %+v", res.Content)
	}
	es, err := a.Store.Train()
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 1 || es[0].State != app.TrainSuperseded {
		t.Fatalf("train = %+v, want t1 superseded", es)
	}
}
