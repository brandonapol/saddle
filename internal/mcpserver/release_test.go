package mcpserver

import (
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

// #411: the orchestrator's release of another task's claim used to delete
// nothing (it matched on the caller) yet answer "released".
func TestReleaseTool(t *testing.T) {
	a, _ := stackSetup(t)
	for _, id := range []string{"t2", "t3"} {
		if _, err := a.Spawn(app.SpawnReq{ID: id, Title: id, Claims: []string{"internal/" + id + "/**"}}); err != nil {
			t.Fatal(err)
		}
	}
	held := func(task, glob string) bool {
		t.Helper()
		all, err := a.Store.Claims()
		if err != nil {
			t.Fatal(err)
		}
		return slices.Contains(all[task], glob)
	}

	// A no-op release says so.
	res := callTool(t, a, app.OrchestratorID, "release", map[string]any{"paths": []string{"internal/t2/**"}}, nil)
	if !res.IsError || !strings.Contains(errText(res), "nothing") {
		t.Fatalf("no-op release = %q (error %v), want it to say nothing was released", errText(res), res.IsError)
	}

	// Only the orchestrator may release another task's claim.
	res = callTool(t, a, "t3", "release", map[string]any{"task": "t2", "paths": []string{"internal/t2/**"}}, nil)
	if !res.IsError || !held("t2", "internal/t2/**") {
		t.Fatalf("t3 released t2's claim: %q", errText(res))
	}

	var out OK
	res = callTool(t, a, app.OrchestratorID, "release", map[string]any{"task": "t2", "paths": []string{"internal/t2/**"}}, &out)
	if res.IsError || held("t2", "internal/t2/**") {
		t.Fatalf("orchestrator release of t2's claim: %q, still held %v", errText(res), held("t2", "internal/t2/**"))
	}
	if !strings.Contains(out.Message, "1") {
		t.Fatalf("message %q doesn't report the count", out.Message)
	}

	// A task still releases its own claims.
	res = callTool(t, a, "t3", "release", map[string]any{"paths": []string{"internal/t3/**"}}, nil)
	if res.IsError || held("t3", "internal/t3/**") {
		t.Fatalf("own release: %q", errText(res))
	}
}
