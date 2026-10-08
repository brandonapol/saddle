package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/autopilot"
)

// #256: saddle autopilot on|off|status|pause|resume.
func TestAutopilotCommands(t *testing.T) {
	a := automergeRepo(t)
	must := func(args ...string) string {
		t.Helper()
		out, err := runCmd(t, autopilotCmd(), args...)
		if err != nil {
			t.Fatalf("saddle autopilot %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	if out := must("status"); !strings.Contains(out, "autopilot: off") {
		t.Fatalf("status before on:\n%s", out)
	}
	if _, err := runCmd(t, autopilotCmd(), "pause"); err == nil {
		t.Fatal("pause while off succeeded")
	}
	if _, err := runCmd(t, autopilotCmd(), "on", "--until-usage", "150%"); err == nil {
		t.Fatal("on accepted --until-usage 150%")
	}
	out := must("on", "--until", "07:00", "--until-usage", "90%", "--max-tasks", "5", "--ready-label", "go")
	if !strings.Contains(out, "autopilot is on") || !strings.Contains(out, "max 5 tasks") {
		t.Fatalf("on:\n%s", out)
	}
	st, err := a.AutopilotState()
	if err != nil {
		t.Fatal(err)
	}
	if !st.On || st.Stop.MaxTasks != 5 || st.Stop.UntilUsage != 0.9 || st.Stop.Until.Format("15:04") != "07:00" || st.Label() != "go" {
		t.Fatalf("state after on = %+v", st)
	}
	if out := must("status"); !strings.Contains(out, "autopilot: on") || !strings.Contains(out, "ready label go") {
		t.Fatalf("status:\n%s", out)
	}
	if out := must("pause"); !strings.Contains(out, "paused") {
		t.Fatalf("pause:\n%s", out)
	}
	if out := must("status"); !strings.Contains(out, "paused") {
		t.Fatalf("status while paused:\n%s", out)
	}
	must("resume")
	var js autopilot.State
	if err := json.Unmarshal([]byte(must("status", "--json")), &js); err != nil || !js.On || js.Paused {
		t.Fatalf("status --json = %+v, %v", js, err)
	}
	out = must("off")
	if !strings.Contains(out, "autopilot stopped: turned off") {
		t.Fatalf("off:\n%s", out)
	}
	if out := must("status"); !strings.Contains(out, "last run: autopilot stopped: turned off") {
		t.Fatalf("status after off:\n%s", out)
	}
}
