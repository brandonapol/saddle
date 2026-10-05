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

// #220: the orchestrator's settings pre-approve the tools it is meant to use,
// so the auto-mode classifier doesn't block them, and git push stays out of
// the model's hands (saddle publish pushes instead).
func TestOrchestratorAllowlist(t *testing.T) {
	l := Launch{Bin: "/opt/bin/saddle", Task: "t0", Cmd: "claude", RunDir: t.TempDir(), Allow: OrchestratorAllow(), Deny: OrchestratorDeny()}
	if _, err := l.Write(); err != nil {
		t.Fatal(err)
	}
	allow := settingsAllow(t, l.RunDir)
	for _, rule := range []string{"Bash(saddle:*)", "Bash(/opt/bin/saddle:*)", "mcp__saddle",
		"Bash(gh pr create:*)", "Bash(gh pr edit:*)", "Bash(gh pr view:*)", "Bash(gh pr list:*)", "Bash(gh pr merge:*)",
		"Bash(gh issue:*)", "Bash(git fetch:*)", "Bash(git log:*)", "Bash(git show:*)", "Bash(git rev-parse:*)"} {
		if !slices.Contains(allow, rule) {
			t.Errorf("orchestrator allowlist lacks %s: %v", rule, allow)
		}
	}
	for _, r := range allow {
		if strings.Contains(r, "git push") {
			t.Errorf("orchestrator may run %s; publishing goes through saddle publish", r)
		}
	}
	if !slices.Contains(OrchestratorDeny(), "Bash(git push:*)") {
		t.Error("git push is not denied to the orchestrator")
	}
	// Workers keep their own list.
	w := Launch{Bin: "/opt/bin/saddle", Task: "t1", Cmd: "claude", RunDir: t.TempDir()}
	if _, err := w.Write(); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(settingsAllow(t, w.RunDir), "Bash(saddle:*)") {
		t.Error("workers got the orchestrator's saddle allow rule")
	}
}

func settingsAllow(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Permissions struct {
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	return s.Permissions.Allow
}
