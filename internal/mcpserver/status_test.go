package mcpserver

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func callStatus(t *testing.T, a *app.App) StatusOut {
	t.Helper()
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
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "status"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("status failed: %+v", res.Content)
	}
	var out StatusOut
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStatusShowsFlaggedStack(t *testing.T) {
	a, _ := stackSetup(t)
	if out := callStatus(t, a); out.StackAtRisk != nil {
		t.Fatalf("unflagged stack reported at risk: %+v", out.StackAtRisk)
	}

	pr := "https://github.com/o/r/pull/1"
	if err := a.SetFlag(app.StackFlag{Task: "t1", Cause: "GitHub reports its PR conflicts with its base", PRs: []string{pr}}); err != nil {
		t.Fatal(err)
	}
	out := callStatus(t, a)
	r := out.StackAtRisk
	if r == nil || r.Task != "t1" || !strings.Contains(r.Cause, "conflicts") || !slices.Equal(r.PRs, []string{pr}) {
		t.Fatalf("stack_at_risk = %+v", r)
	}
	if !strings.Contains(r.Fix, "restack") {
		t.Fatalf("fix = %q, want it to say run restack", r.Fix)
	}
}
