package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/app"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// callTool calls a tool as task and decodes its structured output into out.
func callTool(t *testing.T, a *app.App, task, name string, args, out any) *mcp.CallToolResult {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := New(a, task).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if out != nil && res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatal(err)
		}
	}
	return res
}

func errText(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

func TestSpawnToolPicksAdapter(t *testing.T) {
	a, _ := stackSetup(t)
	var out SpawnOut
	res := callTool(t, a, "t1", "spawn", map[string]any{"title": "hero art", "prompt": "draw", "adapter": "grok"}, &out)
	if res.IsError {
		t.Fatalf("spawn failed: %s", errText(res))
	}
	if got := agent.Recorded(a.Root + "/.saddle/run/" + out.ID); got != "grok" {
		t.Errorf("adapter = %q", got)
	}
}
