package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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

// The PreToolUse hook sees Bash so it can deny git commit --no-verify (#212).
func TestWorkerPreToolUseHookSeesBash(t *testing.T) {
	l := Launch{Bin: "/bin/saddle", Task: "t1", Cmd: "claude", RunDir: t.TempDir()}
	if _, err := l.Write(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(l.RunDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	m := s.Hooks["PreToolUse"][0].Matcher
	for _, tool := range []string{"Bash", "Edit", "Write"} {
		if !slices.Contains(strings.Split(m, "|"), tool) {
			t.Errorf("PreToolUse matcher %q skips %s", m, tool)
		}
	}
}
