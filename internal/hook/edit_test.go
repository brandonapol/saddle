package hook

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
)

func runRaw(t *testing.T, a *app.App, task string, in map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(in)
	var out bytes.Buffer
	if err := Run(a, task, bytes.NewReader(b), &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// #588: with no symbol rules registered, the PreToolUse output is the same
// bytes in both modes, and is CheckWrite's deny as it was before the seam.
func TestPreToolUseUnchangedWithoutSymbolRules(t *testing.T) {
	defer app.SwapSymbolRules(nil, nil)()
	a := setup(t)
	t1, err := a.Spawn(app.SpawnReq{Title: "one", Claims: []string{"a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	t2, _ := a.Spawn(app.SpawnReq{Title: "two"})
	claimed := filepath.Join(t2.Worktree, "a.go")
	calls := []map[string]any{
		{"hook_event_name": "PreToolUse", "tool_name": "Edit",
			"tool_input": map[string]any{"file_path": claimed, "old_string": "package a", "new_string": "package b"}},
		{"hook_event_name": "PreToolUse", "tool_name": "Write",
			"tool_input": map[string]any{"file_path": filepath.Join(t2.Worktree, "b.go"), "content": "package a\n"}},
		{"hook_event_name": "PreToolUse", "tool_name": "NotebookEdit",
			"tool_input": map[string]any{"notebook_path": claimed}},
	}
	byMode := map[string][]string{}
	for _, mode := range config.Granularities() {
		a.Cfg.Claims.Granularity = mode
		for _, c := range calls {
			byMode[mode] = append(byMode[mode], runRaw(t, a, t2.ID, c))
		}
	}
	for i := range calls {
		if f, s := byMode[config.GranularityFile][i], byMode[config.GranularitySymbol][i]; f != s {
			t.Errorf("call %d: file mode %q, symbol mode %q", i, f, s)
		}
	}
	d := a.CheckWrite(t2.ID, claimed)
	var want bytes.Buffer
	if err := json.NewEncoder(&want).Encode(deny(Input{}, RuleWriteGuard, d.Reason+ownerDiff(a, t2.ID, claimed))); err != nil {
		t.Fatal(err)
	}
	if got := byMode[config.GranularityFile][0]; got != want.String() || !strings.Contains(got, t1.ID) {
		t.Errorf("deny = %q, want %q", got, want.String())
	}
	if got := byMode[config.GranularityFile][1]; got != "" {
		t.Errorf("allowed write printed %q", got)
	}
}

// #588: a symbol rule gets the hook's proposed content and can deny; in file
// mode the hook never runs it. A note rule's text reaches PostToolUse's
// additionalContext with the file's content before and after.
func TestHookRunsSymbolRulesInSymbolMode(t *testing.T) {
	var seen []app.Edit
	var noted []app.Edit
	defer app.SwapSymbolRules([]app.EditRule{{Name: "test", Check: func(_ *app.App, e app.Edit, d app.Decision) app.Decision {
		seen = append(seen, e)
		if strings.Contains(string(e.After), "forbidden") {
			return app.Decision{Reason: "test rule says no"}
		}
		return d
	}}}, []app.NoteRule{{Name: "test", Note: func(_ *app.App, e app.Edit) string {
		noted = append(noted, e)
		return "note on " + e.Rel
	}}})()
	a := setup(t)
	t2, err := a.Spawn(app.SpawnReq{Title: "two"})
	if err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(t2.Worktree, "a.go")
	pre := map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": f, "old_string": "package a", "new_string": "package forbidden"}}
	post := map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Edit",
		"tool_input":    map[string]any{"file_path": f},
		"tool_response": map[string]any{"filePath": f, "originalFile": "package old\n"}}

	a.Cfg.Claims.Granularity = config.GranularityFile
	if out := run(t, a, t2.ID, pre); out != nil {
		t.Fatalf("file mode: out = %v, want allowed", out)
	}
	if out := run(t, a, t2.ID, post); out != nil {
		t.Fatalf("file mode: post out = %v", out)
	}
	if len(seen)+len(noted) != 0 {
		t.Fatalf("file mode ran rules: %v %v", seen, noted)
	}

	a.Cfg.Claims.Granularity = config.GranularitySymbol
	out := run(t, a, t2.ID, pre)
	hs, _ := out["hookSpecificOutput"].(map[string]any)
	if hs["permissionDecision"] != "deny" || !strings.Contains(hs["permissionDecisionReason"].(string), "test rule says no") {
		t.Fatalf("symbol mode: out = %v, want the rule's deny", out)
	}
	if len(seen) != 1 || !seen[0].Known || string(seen[0].Before) != "package a\n" ||
		string(seen[0].After) != "package forbidden\n" || seen[0].Rel != "a.go" {
		t.Fatalf("rule saw %+v", seen)
	}
	out = run(t, a, t2.ID, post)
	hs, _ = out["hookSpecificOutput"].(map[string]any)
	if hs["hookEventName"] != "PostToolUse" || hs["additionalContext"] != "note on a.go" {
		t.Fatalf("post out = %v", out)
	}
	if len(noted) != 1 || !noted[0].Known || string(noted[0].Before) != "package old\n" || string(noted[0].After) != "package a\n" {
		t.Fatalf("note rule saw %+v", noted)
	}
	// Without the original in tool_response the edit is unknown, and the
	// rule still runs so it can fall back.
	delete(post, "tool_response")
	run(t, a, t2.ID, post)
	if len(noted) != 2 || noted[1].Known {
		t.Fatalf("note rule saw %+v, want an unknown edit", noted[1:])
	}
}
