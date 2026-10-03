package e2e

import (
	"context"
	"os/exec"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP calls a tool on `saddle mcp` as task (t0 is the orchestrator) and
// returns its text output and whether it was an error result.
func (w *World) MCP(task, tool string, args map[string]any) (string, bool) {
	w.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), Timeout())
	defer cancel()
	cmd := exec.Command(w.Bins.Saddle, "mcp")
	cmd.Dir, cmd.Env = w.Repo, append(w.Env(), "SADDLE_TASK="+task)
	c := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "e2e"}, nil)
	s, err := c.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	must(w.T, err)
	defer func() { _ = s.Close() }()
	if args == nil {
		args = map[string]any{}
	}
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	must(w.T, err)
	var text []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			text = append(text, t.Text)
		}
	}
	return strings.Join(text, "\n"), res.IsError
}

// MustMCP calls a tool and fails the test on an error result.
func (w *World) MustMCP(task, tool string, args map[string]any) string {
	w.T.Helper()
	out, isErr := w.MCP(task, tool, args)
	if isErr {
		w.T.Fatalf("mcp %s as %s failed: %s", tool, task, out)
	}
	return out
}
