package mcpserver

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

// #176: the concurrency tool reads, sets and resets the worker cap, and
// spawn refuses over it with a message naming the way out.
func TestConcurrencyTool(t *testing.T) {
	a, _ := stackSetup(t)
	a.Cfg.Concurrency = 5
	var out app.Concurrency
	if res := callTool(t, a, app.OrchestratorID, "concurrency", map[string]any{}, &out); res.IsError || out.Limit != 5 || out.Source != app.ConcurrencyFromConfig {
		t.Fatalf("read: %s %+v", errText(res), out)
	}
	if res := callTool(t, a, app.OrchestratorID, "concurrency", map[string]any{"limit": 1}, &out); res.IsError || out.Limit != 1 || out.Source != app.ConcurrencyRuntime {
		t.Fatalf("set 1: %s %+v", errText(res), out)
	}
	if res := callTool(t, a, app.OrchestratorID, "concurrency", map[string]any{"limit": 17}, nil); !res.IsError {
		t.Fatal("limit 17 accepted")
	}
	running := out.Running
	for i := running; i < 1; i++ {
		if _, err := a.Spawn(app.SpawnReq{Title: "fill"}); err != nil {
			t.Fatal(err)
		}
	}
	res := callTool(t, a, app.OrchestratorID, "spawn", map[string]any{"title": "over", "prompt": "x"}, nil)
	if !res.IsError || !strings.Contains(errText(res), "concurrency cap") || !strings.Contains(errText(res), "concurrency") {
		t.Fatalf("spawn over the cap: %s", errText(res))
	}
	if res := callTool(t, a, app.OrchestratorID, "concurrency", map[string]any{"reset": true}, &out); res.IsError || out.Limit != 5 {
		t.Fatalf("reset: %s %+v", errText(res), out)
	}
}
