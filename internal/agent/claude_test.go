package agent

import (
	"os"
	"strings"
	"testing"
)

func TestOrchestratorCannotMoveBranches(t *testing.T) {
	l := Launch{Bin: "/bin/saddle", Task: "t0", Cmd: "claude", RunDir: t.TempDir(), Deny: OrchestratorDeny()}
	cmd, err := l.Headless("")
	if err != nil {
		t.Fatal(err)
	}
	sh, err := l.Write()
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(strings.Trim(strings.TrimPrefix(sh, "bash "), "'"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	for _, git := range []string{"merge", "rebase", "reset", "push", "branch -f", "update-ref", "checkout"} {
		rule := "Bash(git " + git + ":*)"
		if !strings.Contains(args, rule) {
			t.Errorf("headless orchestrator may still run git %s", git)
		}
		if !strings.Contains(string(script), rule) {
			t.Errorf("tmux orchestrator may still run git %s", git)
		}
	}
}
