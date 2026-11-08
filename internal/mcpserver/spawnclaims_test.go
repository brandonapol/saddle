package mcpserver

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

// #908: spawn issue=N with no claims takes the ticket's Claims: line
// instead of the orchestrator guessing globs; explicit claims still win.
func TestSpawnIssueTakesTicketClaims(t *testing.T) {
	a, _ := stackSetup(t)
	bin := t.TempDir()
	gh := "#!/bin/sh\ncase \"$1 $2 $3\" in\n" +
		"\"issue view 7\") echo '{\"number\":7,\"body\":\"Fix it.\\n\\nClaims: text-game/src/queries*\\n\"}' ;;\n" +
		"*) echo \"no such issue\" >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(gh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out SpawnOut
	res := callTool(t, a, app.OrchestratorID, "spawn", map[string]any{"title": "queries (#7)", "prompt": "x", "issue": 7}, &out)
	if res.IsError {
		t.Fatalf("spawn: %s", errText(res))
	}
	if !slices.Equal(out.Claims, []string{"text-game/src/queries*"}) || !strings.Contains(out.ClaimsNote, "Claims: line") {
		t.Fatalf("claims = %v, note %q", out.Claims, out.ClaimsNote)
	}
	cl, err := a.Store.Claims()
	if err != nil || !slices.Equal(cl[out.ID], []string{"text-game/src/queries*"}) {
		t.Fatalf("stored claims = %v, %v", cl[out.ID], err)
	}

	out = SpawnOut{}
	res = callTool(t, a, app.OrchestratorID, "spawn", map[string]any{"title": "mine (#7)", "prompt": "x", "issue": 7, "claims": []string{"other/**"}}, &out)
	if res.IsError || !slices.Equal(out.Claims, []string{"other/**"}) || out.ClaimsNote != "" {
		t.Fatalf("explicit claims: %v %v %q", errText(res), out.Claims, out.ClaimsNote)
	}
}
