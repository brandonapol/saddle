package refguard

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestOperatorOverrideIsExplicitAndUnavailableToTasks(t *testing.T) {
	for _, tc := range []struct{ task, override, want string }{
		{"", "", Unknown}, {"", "off", "operator"}, {"t1", "off", "t1"}, {"t0", "off", "t0"},
	} {
		getenv := func(k string) string {
			switch k { case "SADDLE_TASK": return tc.task; case "SADDLE_REFGUARD": return tc.override }
			return ""
		}
		if got := Actor(getenv); got != tc.want { t.Errorf("Actor(%+v) = %q", tc, got) }
	}
}

func TestOperatorOverrideMovesAndPushesWithAudit(t *testing.T) {
	r := setup(t)
	r.git("t1", "branch", "saddle/t1-one", "HEAD~")
	if err := r.run("", "update-ref", "refs/heads/saddle/t1-one", r.rev("HEAD")); err == nil || !strings.Contains(err.Error(), "SADDLE_REFGUARD=off") {
		t.Fatalf("denial must explain recovery: %v", err)
	}
	for _, args := range [][]string{
		{"update-ref", "refs/heads/saddle/t1-one", r.rev("HEAD")},
		{"-c", "alias.testpush=!printf 'refs/heads/saddle/t1-one abc refs/heads/saddle/t1-one def\\n' | \"$1\" refguard pre-push", "testpush", r.bin},
	} {
		cmd := exec.Command("git", append([]string{"-C", r.root}, args...)...)
		cmd.Env = append(os.Environ(), "SADDLE_TASK=", "SADDLE_TRAIN=", "SADDLE_REFGUARD=off")
		if out, err := cmd.CombinedOutput(); err != nil { t.Fatalf("operator command: %v: %s", err, out) }
	}
	events, err := r.st.Events(20)
	if err != nil { t.Fatal(err) }
	n := 0
	for _, e := range events {
		var data Event
		if json.Unmarshal([]byte(e.Data), &data) == nil && data.Actor == "operator" && data.Denied == "" { n++ }
	}
	if n != 2 { t.Fatalf("operator audit entries = %d, want 2: %+v", n, events) }
}
