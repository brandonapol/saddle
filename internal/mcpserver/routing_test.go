package mcpserver

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/app"
)

// #505: spawn takes difficulty and needs, and status says which adapter
// the chooser put each task on and why.
func TestSpawnDifficultyShowsPickInStatus(t *testing.T) {
	a, _ := stackSetup(t)
	a.AdapterStatus = func() []agent.Status {
		return []agent.Status{{Name: "claude", OK: true}, {Name: "grok", OK: true}, {Name: "codex", OK: true}}
	}
	var out SpawnOut
	if res := callTool(t, a, app.OrchestratorID, "spawn", map[string]any{"title": "design", "prompt": "x", "difficulty": "hard-design", "needs": []string{"review"}}, &out); res.IsError {
		t.Fatalf("spawn failed: %+v", res.Content)
	}
	for _, tv := range callStatus(t, a).Tasks {
		if tv.ID != out.ID {
			continue
		}
		if !strings.HasPrefix(tv.Pick, "claude (opus): has review, suits hard") {
			t.Fatalf("task = %+v", tv)
		}
		return
	}
	t.Fatalf("status lacks %s", out.ID)
}
