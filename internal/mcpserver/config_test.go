package mcpserver

import (
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/usage"
)

// #908: the orchestrator's edit of .saddle/config.toml was denied and it
// had no tool for it. The config tool reads and sets a key; only the
// orchestrator may set one.
func TestConfigTool(t *testing.T) {
	a, _ := stackSetup(t)
	var out ConfigOut
	if res := callTool(t, a, "t1", "config", map[string]any{"key": "adapters.grok.disabled", "value": "true"}, nil); !res.IsError {
		t.Fatal("a worker changed the config")
	}
	if res := callTool(t, a, app.OrchestratorID, "config", map[string]any{"key": "adapters.grok.disabled", "value": "true"}, &out); res.IsError || out.Value != "true" || out.Line != "disabled = true" {
		t.Fatalf("set: %+v %+v", res, out)
	}
	if !a.Cfg.AdapterDisabled(usage.Grok) {
		t.Error("the server didn't apply it")
	}
	out = ConfigOut{}
	if res := callTool(t, a, "t1", "config", map[string]any{"key": "adapters.grok.disabled"}, &out); res.IsError || out.Value != "true" {
		t.Fatalf("read: %+v %+v", res, out)
	}
	if res := callTool(t, a, app.OrchestratorID, "config", map[string]any{"key": "adapters.grok.disabeld", "value": "true"}, nil); !res.IsError {
		t.Fatal("a misspelt key was accepted")
	}
}
