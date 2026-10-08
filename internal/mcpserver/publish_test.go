package mcpserver

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

// #220: publish is an MCP tool too, so the orchestrator never pushes or
// opens a PR by hand when prs is blocked.
func TestPublishTool(t *testing.T) {
	a, _ := stackSetup(t)
	var out OK
	if res := callTool(t, a, app.OrchestratorID, "publish", map[string]any{"target": "t9"}, &out); !res.IsError {
		t.Fatal("published a task that doesn't exist")
	}
	const pr = "https://github.com/o/r/pull/7"
	if err := a.Store.SetField("t1", "pr", pr); err != nil {
		t.Fatal(err)
	}
	if res := callTool(t, a, app.OrchestratorID, "publish", map[string]any{"target": "t1"}, &out); res.IsError {
		t.Fatalf("publish: %s", errText(res))
	}
	if out.Message != "PR already open: "+pr {
		t.Fatalf("message = %q", out.Message)
	}
}

// The server's instructions name publish as the escape hatch.
func TestInstructionsNamePublish(t *testing.T) {
	if !strings.Contains(serverInstructions, "publish is the escape hatch") {
		t.Fatalf("instructions: %q", serverInstructions)
	}
}

// #213: sentinel_ack acknowledges ci-red holds too, even with no stack
// flag, and status lists them.
func TestSentinelAckAcksCIRed(t *testing.T) {
	a, _ := stackSetup(t)
	if err := a.SetCIRed(app.CIRedState{Red: []app.CIRedLayer{{Task: "t1", PR: "https://github.com/o/r/pull/1", Head: "abc", Checks: []string{"ci / test"}}}}); err != nil {
		t.Fatal(err)
	}
	st := callStatus(t, a)
	if len(st.CIRed) != 1 || st.CIRed[0].Task != "t1" || st.CIRed[0].Acked {
		t.Fatalf("status ci_red = %+v, want t1 unacked", st.CIRed)
	}
	var out OK
	if res := callTool(t, a, app.OrchestratorID, "sentinel_ack", nil, &out); res.IsError {
		t.Fatalf("sentinel_ack: %s", errText(res))
	}
	if !strings.Contains(out.Message, "ci-red on t1") {
		t.Fatalf("message = %q", out.Message)
	}
	if st := callStatus(t, a); len(st.CIRed) != 1 || !st.CIRed[0].Acked {
		t.Fatalf("status ci_red = %+v, want t1 acked", st.CIRed)
	}
}
