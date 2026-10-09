package remote

import (
	"context"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/mcpserver"
)

func TestAllows(t *testing.T) {
	cases := []struct {
		granted []Scope
		tool    string
		want    bool
	}{
		{[]Scope{ScopeRead}, "status", true},
		{[]Scope{ScopeRead}, "needs_you", true},
		{[]Scope{ScopeRead}, "land", false},
		{[]Scope{ScopeRead}, "send_keys", false},
		{[]Scope{ScopeRead, ScopeAct}, "land", false},
		{[]Scope{ScopeRead, ScopeAct}, "queue_hold", true},
		{[]Scope{ScopeRead, ScopeLand}, "land", true},
		{[]Scope{ScopeAdmin}, "kill", true},
		{[]Scope{ScopeAdmin}, "land", true},
		// Worker-only tools are never reachable remotely, not even with admin.
		{[]Scope{ScopeAdmin}, "done", false},
		{[]Scope{ScopeAdmin}, "claim", false},
		// Unknown tools are denied: the table is an allowlist.
		{[]Scope{ScopeAdmin}, "rm_rf", false},
		{nil, "status", false},
	}
	for _, c := range cases {
		if got := Allows(c.granted, c.tool); got != c.want {
			t.Errorf("Allows(%v, %q) = %v, want %v", c.granted, c.tool, got, c.want)
		}
	}
}

// TestEveryMCPToolHasAScope keeps the scope table in step with the MCP
// server: a new orchestrator tool must be given a scope before it can be
// reached remotely, and the table must not list tools that are gone.
func TestEveryMCPToolHasAScope(t *testing.T) {
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := mcpserver.New(nil, app.OrchestratorID).Connect(ctx, st, nil); err != nil {
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
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
		if _, ok := ToolScopes[tl.Name]; !ok {
			t.Errorf("MCP tool %q has no entry in ToolScopes", tl.Name)
		}
	}
	for name := range ToolScopes {
		if !slices.Contains(names, name) && !slices.Contains(RemoteOnlyTools, name) {
			t.Errorf("ToolScopes lists %q, which is neither an MCP tool nor a remote-only tool", name)
		}
	}
}

func TestParseScopes(t *testing.T) {
	got, err := ParseScopes("act")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []Scope{ScopeRead, ScopeAct}) {
		t.Fatalf("ParseScopes(act) = %v, want read added", got)
	}
	if _, err := ParseScopes("read,local"); err == nil {
		t.Fatal("ParseScopes accepted the local pseudo-scope")
	}
	if _, err := ParseScopes("root"); err == nil {
		t.Fatal("ParseScopes accepted an unknown scope")
	}
}
