package mcpserver

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
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

func TestBriefTool(t *testing.T) {
	a, _ := stackSetup(t)
	a.Cfg.Serial = []string{"go.sum"}
	p, err := a.Spawn(app.SpawnReq{ID: "t2", Title: "api", Prompt: "build the api", Claims: []string{"api/**"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Spawn(app.SpawnReq{ID: "t3", Title: "docs", Claims: []string{"docs/**"}, Parent: p.ID}); err != nil {
		t.Fatal(err)
	}
	var out BriefOut
	res := callTool(t, a, "t2", "brief", map[string]any{}, &out)
	if res.IsError {
		t.Fatalf("brief failed: %s", errText(res))
	}
	if out.Task != "t2" || out.Title != "api" || out.Goal != "build the api" || out.Adapter != "claude" || out.Branch != p.Branch {
		t.Errorf("brief = %+v", out)
	}
	if len(out.Claims) != 1 || out.Claims[0] != "api/**" {
		t.Errorf("claims = %v", out.Claims)
	}
	if got := out.DoNotTouch["t3"]; len(got) != 1 || got[0] != "docs/**" {
		t.Errorf("do_not_touch = %v", out.DoNotTouch)
	}
	if _, ok := out.DoNotTouch["t2"]; ok {
		t.Error("own claims listed as do-not-touch")
	}
	if len(out.Serial) != 1 || len(out.Children) != 1 || out.Children[0].ID != "t3" || out.DoneWhen == "" {
		t.Errorf("brief = %+v", out)
	}
	if res := callTool(t, a, "", "brief", map[string]any{}, nil); !res.IsError {
		t.Error("brief without SADDLE_TASK should fail")
	}
}

func TestAskOwnerTool(t *testing.T) {
	a, _ := stackSetup(t)
	a.Cfg.Serial = []string{"go.sum"}
	if _, err := a.Spawn(app.SpawnReq{ID: "t2", Title: "api", Claims: []string{"api/**"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Spawn(app.SpawnReq{ID: "t3", Title: "docs"}); err != nil {
		t.Fatal(err)
	}
	pending := func(task string) string {
		ns, err := a.Store.TakeNotices(task, false)
		if err != nil {
			t.Fatal(err)
		}
		return store.FormatNotices(ns)
	}
	_ = pending(app.OrchestratorID)

	var out AskOut
	res := callTool(t, a, "t3", "ask_owner", map[string]any{"path": "api/server.go", "question": "can I add a field to Config?"}, &out)
	if res.IsError || out.Owner != "t2" {
		t.Fatalf("ask_owner = %+v %s", out, errText(res))
	}
	if got := pending("t2"); !strings.Contains(got, "t3") || !strings.Contains(got, "api/server.go") || !strings.Contains(got, "add a field") {
		t.Errorf("owner notice = %q", got)
	}
	// Serial files belong to the merge train; the orchestrator answers.
	res = callTool(t, a, "t3", "ask_owner", map[string]any{"path": "go.sum", "question": "bump?"}, &out)
	if res.IsError || out.Owner != app.OrchestratorID || !strings.Contains(pending(app.OrchestratorID), "bump?") {
		t.Fatalf("serial ask = %+v %s", out, errText(res))
	}
	if res := callTool(t, a, "t3", "ask_owner", map[string]any{"path": "nobody/x.go", "question": "?"}, nil); !res.IsError || !strings.Contains(errText(res), "claim") {
		t.Errorf("unowned path: %s", errText(res))
	}
	if res := callTool(t, a, "t2", "ask_owner", map[string]any{"path": "api/x.go", "question": "?"}, nil); !res.IsError {
		t.Error("asking yourself should fail")
	}
}

// #193: spawn's after arg reaches the app, so the task stacks on t7.
func TestSpawnToolAfter(t *testing.T) {
	a, _ := stackSetup(t)
	if _, err := a.Spawn(app.SpawnReq{ID: "t7", Title: "harness"}); err != nil {
		t.Fatal(err)
	}
	var out SpawnOut
	res := callTool(t, a, app.OrchestratorID, "spawn", map[string]any{"title": "ci job", "prompt": "x", "after": []string{"t7"}}, &out)
	if res.IsError {
		t.Fatalf("spawn failed: %s", errText(res))
	}
	if got := a.TaskAfter(out.ID); len(got) != 1 || got[0] != "t7" {
		t.Fatalf("after = %v, want [t7]", got)
	}
}
