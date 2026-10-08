package mcpserver

import (
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/autopilot"
)

// #256: the autopilot tool turns the driver on and off, pauses and resumes
// it, and reports its state.
func TestAutopilotTool(t *testing.T) {
	a, _ := stackSetup(t)
	noGH(t)
	call := func(args map[string]any) (autopilot.State, bool) {
		var out autopilot.State
		res := callTool(t, a, app.OrchestratorID, "autopilot", args, &out)
		if res.IsError {
			t.Logf("%v: %s", args, errText(res))
		}
		return out, !res.IsError
	}
	if st, ok := call(map[string]any{"action": "status"}); !ok || st.On {
		t.Fatalf("status: %+v", st)
	}
	st, ok := call(map[string]any{"action": "on", "max_tasks": 3, "ready_label": "go", "until_usage": "90%"})
	if !ok || !st.On || st.Stop.MaxTasks != 3 || st.Label() != "go" || st.Stop.UntilUsage != 0.9 {
		t.Fatalf("on: %+v", st)
	}
	if st, ok := call(map[string]any{"action": "pause"}); !ok || !st.Paused {
		t.Fatalf("pause: %+v", st)
	}
	if st, ok := call(map[string]any{"action": "resume"}); !ok || st.Paused || !st.On {
		t.Fatalf("resume: %+v", st)
	}
	if st, ok := call(map[string]any{"action": "off"}); !ok || st.On {
		t.Fatalf("off: %+v", st)
	}
	if _, ok := call(map[string]any{"action": "on", "until": "not a time"}); ok {
		t.Fatal("bad until succeeded")
	}
	if _, ok := call(map[string]any{"action": "fly"}); ok {
		t.Fatal("unknown action succeeded")
	}
}
