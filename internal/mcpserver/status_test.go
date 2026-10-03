package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

// The restack tool tells the orchestrator what the sentinel's notice and its
// brief do: a flagged stack is fixed with restack, never with git.
func TestRestackToolIsTheFixForAFlaggedStack(t *testing.T) {
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := New(nil, app.OrchestratorID).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "restack" {
			continue
		}
		for _, want := range []string{"stack_at_risk", "never with git"} {
			if !strings.Contains(tool.Description, want) {
				t.Fatalf("restack description lacks %q: %s", want, tool.Description)
			}
		}
		return
	}
	t.Fatal("no restack tool")
}

// #179: status reports how full the orchestrator's context is, against its
// compact_at threshold, once its transcript has a reading.
func TestStatusShowsOrchestratorContext(t *testing.T) {
	a, _ := stackSetup(t)
	cfgDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfgDir)
	if _, err := a.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	if out := callStatus(t, a); out.OrchestratorContext != nil {
		t.Fatalf("no session yet, but context = %+v", out.OrchestratorContext)
	}
	if err := a.Store.SetField(app.OrchestratorID, "session_id", "s1"); err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","timestamp":"2026-10-03T10:00:00Z","message":{"id":"m1","model":"claude-opus-4-5","usage":{"input_tokens":1,"output_tokens":5,"cache_read_input_tokens":89999}}}` + "\n"
	if err := os.MkdirAll(filepath.Join(cfgDir, "projects", "-x"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, cfgDir, "projects/-x/s1.jsonl", line)
	c := callStatus(t, a).OrchestratorContext
	if c == nil || c.Percent != 45 || c.Tokens != 90_000 || c.Window != 200_000 || c.CompactAt != 70 {
		t.Fatalf("orchestrator_context = %+v, want 45%% of 200000 with compact_at 70", c)
	}
}
