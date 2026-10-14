package hook

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

// #532: opencode's claims plugin sends each tool call as a PreToolUse
// payload: opencode's tool name, its args as tool_input, and the session's
// directory as cwd. write and edit name the file filePath, absolute or
// relative to cwd; apply_patch names its files in patchText. A file another
// task claims is denied, naming the owner; an unclaimed one is allowed.
func TestOpenCodePluginPayloadEnforcesClaims(t *testing.T) {
	a := setup(t)
	t1, err := a.Spawn(app.SpawnReq{Title: "one", Claims: []string{"a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	t2, _ := a.Spawn(app.SpawnReq{Title: "two"})
	call := func(tool string, args map[string]any) map[string]any {
		return run(t, a, t2.ID, map[string]any{
			"hook_event_name": "PreToolUse", "tool_name": tool, "cwd": t2.Worktree, "tool_input": args,
		})
	}
	denied := map[string]map[string]any{
		"write absolute": {"tool": "write", "args": map[string]any{"filePath": filepath.Join(t2.Worktree, "a.go"), "content": "x"}},
		"edit relative":  {"tool": "edit", "args": map[string]any{"filePath": "a.go", "oldString": "a", "newString": "b"}},
		"patch update": {"tool": "apply_patch", "args": map[string]any{"patchText": "*** Begin Patch\n" +
			"*** Add File: new.go\n+package a\n*** Update File: a.go\n@@\n-package a\n+package b\n*** End Patch\n"}},
		"patch move": {"tool": "apply_patch", "args": map[string]any{"patchText": "*** Begin Patch\n" +
			"*** Update File: c.go\n*** Move to: a.go\n@@\n-x\n+y\n*** End Patch\n"}},
		"patch delete": {"tool": "apply_patch", "args": map[string]any{"patchText": "*** Begin Patch\n*** Delete File: " +
			filepath.Join(t2.Worktree, "a.go") + "\n*** End Patch\n"}},
	}
	for name, c := range denied {
		out := call(c["tool"].(string), c["args"].(map[string]any))
		hs, _ := out["hookSpecificOutput"].(map[string]any)
		if hs["permissionDecision"] != "deny" || !strings.Contains(hs["permissionDecisionReason"].(string), t1.ID) {
			t.Errorf("%s: out = %v, want a deny naming %s", name, out, t1.ID)
		}
	}
	allowed := map[string]map[string]any{
		"write unclaimed": {"tool": "write", "args": map[string]any{"filePath": "b.go", "content": "x"}},
		"patch unclaimed": {"tool": "apply_patch", "args": map[string]any{"patchText": "*** Begin Patch\n*** Add File: d.go\n+x\n*** End Patch\n"}},
	}
	for name, c := range allowed {
		if out := call(c["tool"].(string), c["args"].(map[string]any)); out != nil {
			t.Errorf("%s: out = %v, want allowed", name, out)
		}
	}
	// The no-verify rule reads bash's command arg, which opencode names the same.
	out := call("bash", map[string]any{"command": "git commit --no-verify -m x"})
	if hs, _ := out["hookSpecificOutput"].(map[string]any); hs["permissionDecision"] != "deny" {
		t.Errorf("bash --no-verify: out = %v", out)
	}
}

func TestPatchPaths(t *testing.T) {
	got := patchPaths("*** Begin Patch\n*** Add File: a.go\n+x\n*** Update File: b/c.go\n*** Move to: d.go\n" +
		"*** Delete File:  e.go \n+*** Add File: not-a-header.go\n*** End Patch")
	want := []string{"a.go", "b/c.go", "d.go", "e.go"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("patchPaths = %q, want %q", got, want)
	}
}
