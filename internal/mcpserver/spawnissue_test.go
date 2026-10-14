package mcpserver

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

// #557: a spawn whose title or prompt names an issue but leaves issue unset
// opens a PR that closes nothing; the result says so.
func TestSpawnToolWarnsWhenAnIssueIsNamedButNotSet(t *testing.T) {
	a, _ := stackSetup(t)
	var out SpawnOut
	res := callTool(t, a, app.OrchestratorID, "spawn", map[string]any{"title": "fix the train (#42)", "prompt": "See #42 and #7."}, &out)
	if res.IsError {
		t.Fatalf("spawn failed: %s", errText(res))
	}
	if !strings.Contains(out.Warning, "#42") || !strings.Contains(out.Warning, "issue=") {
		t.Fatalf("warning = %q, want one naming #42 and issue=", out.Warning)
	}

	out = SpawnOut{}
	res = callTool(t, a, app.OrchestratorID, "spawn", map[string]any{"title": "fix it again (#43)", "prompt": "x", "issue": 43}, &out)
	if res.IsError {
		t.Fatalf("spawn failed: %s", errText(res))
	}
	if out.Warning != "" {
		t.Fatalf("warning = %q with issue set, want none", out.Warning)
	}

	out = SpawnOut{}
	res = callTool(t, a, app.OrchestratorID, "spawn", map[string]any{"title": "tidy", "prompt": "x"}, &out)
	if res.IsError {
		t.Fatalf("spawn failed: %s", errText(res))
	}
	if out.Warning != "" {
		t.Fatalf("warning = %q with no issue named, want none", out.Warning)
	}
}

func TestIssueRefs(t *testing.T) {
	got := issueRefs("fix (#42)", "See #42, #7 and abc#9 and &#38;")
	if len(got) != 2 || got[0] != 42 || got[1] != 7 {
		t.Fatalf("issueRefs = %v, want [42 7]", got)
	}
}
