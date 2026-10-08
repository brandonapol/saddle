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

// A resumed worker continues its Claude session in a new window (#254).
func TestWriteResumesSession(t *testing.T) {
	l := Launch{Bin: "/bin/saddle", Task: "t1", Cmd: "claude", RunDir: t.TempDir(), Prompt: "carry on", Resume: "abc-123"}
	sh, err := l.Write()
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(strings.Trim(strings.TrimPrefix(sh, "bash "), "'"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), "--resume 'abc-123'") {
		t.Errorf("launch script doesn't resume the session:\n%s", script)
	}
	l.Resume = ""
	if sh, err = l.Write(); err != nil {
		t.Fatal(err)
	}
	script, _ = os.ReadFile(strings.Trim(strings.TrimPrefix(sh, "bash "), "'"))
	if strings.Contains(string(script), "--resume") {
		t.Errorf("a fresh launch resumes:\n%s", script)
	}
}

// Skills and slash commands reach both the headless orchestrator and the
// interactive workers (#255): no launch turns them off or narrows the
// settings sources that hold user and project skills, and the orchestrator
// may call the Skill tool without a prompt (a prompt in -p is a denial).
func TestLaunchesKeepSkills(t *testing.T) {
	for _, l := range []Launch{
		{Bin: "/bin/saddle", Task: "t0", Cmd: "claude", RunDir: t.TempDir(), Allow: OrchestratorAllow(), Deny: OrchestratorDeny()},
		{Bin: "/bin/saddle", Task: "t1", Cmd: "claude", RunDir: t.TempDir()},
	} {
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
		for _, flag := range []string{"--bare", "--disable-slash-commands", "--setting-sources", "--strict-mcp-config"} {
			if slices.Contains(cmd.Args, flag) {
				t.Errorf("%s headless launch passes %s", l.Task, flag)
			}
			if strings.Contains(string(script), flag) {
				t.Errorf("%s tmux launch passes %s", l.Task, flag)
			}
		}
		for _, env := range cmd.Env {
			if strings.HasPrefix(env, "CLAUDE_CONFIG_DIR=") && env != "CLAUDE_CONFIG_DIR="+os.Getenv("CLAUDE_CONFIG_DIR") {
				t.Errorf("%s headless launch overrides %s", l.Task, env)
			}
		}
		for _, deny := range l.Deny {
			if deny == "Skill" || strings.HasPrefix(deny, "Skill(") {
				t.Errorf("%s denies the Skill tool", l.Task)
			}
		}
	}
	if !slices.Contains(OrchestratorAllow(), "Skill") {
		t.Error("the orchestrator's Skill tool calls need a permission prompt it can't answer")
	}
}

// fakeClaude writes a script standing in for claude: it rejects --subagents
// and bad advisor models the way claude 2.1.287 does, and otherwise stops at
// the missing --print input.
func fakeClaude(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	script := `#!/bin/sh
case "$1" in
--subagents) echo "error: unknown option '--subagents'" >&2; echo "(Did you mean --agents?)" >&2; exit 1 ;;
--advisor) if [ "$2" = nope ]; then echo "Error: The model \"nope\" cannot be used as an advisor." >&2; exit 1; fi ;;
esac
echo "Error: Input must be provided either through stdin or as a prompt argument when using --print" >&2
exit 1
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// #257: the probe accepts a flag claude parses and names one it rejects.
func TestProbeFlag(t *testing.T) {
	bin := fakeClaude(t)
	if err := ProbeFlag(bin, "--advisor", "opus"); err != nil {
		t.Errorf("--advisor opus: %v", err)
	}
	for flag, value := range map[string]string{"--subagents": "haiku", "--advisor": "nope"} {
		err := ProbeFlag(bin, flag, value)
		if err == nil || !strings.Contains(err.Error(), flag) {
			t.Errorf("%s %s: err = %v, want one naming the flag", flag, value, err)
		}
	}
	if err := ProbeFlag(filepath.Join(t.TempDir(), "missing"), "--advisor", "opus"); err == nil {
		t.Error("missing binary passed the probe")
	}
}

// #257: advisor fields reach both the headless and the tmux launch; empty
// fields add nothing.
func TestAdvisorLaunchArgs(t *testing.T) {
	l := Launch{Bin: "/bin/saddle", Task: "t0", Cmd: "claude", Model: "sonnet", RunDir: t.TempDir(),
		Effort: "high", Advisor: "opus", SubagentModel: "haiku"}
	cmd, err := l.Headless("")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--model sonnet --effort high --advisor opus") {
		t.Errorf("headless args: %s", args)
	}
	if !slices.Contains(cmd.Env, SubagentModelEnv+"=haiku") {
		t.Errorf("headless env lacks %s", SubagentModelEnv)
	}
	sh, err := l.Write()
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(strings.Trim(strings.TrimPrefix(sh, "bash "), "'"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--model 'sonnet' '--effort' 'high' '--advisor' 'opus'", "export " + SubagentModelEnv + "='haiku'"} {
		if !strings.Contains(string(script), want) {
			t.Errorf("launch.sh lacks %q:\n%s", want, script)
		}
	}

	l.Effort, l.Advisor, l.SubagentModel = "", "", ""
	cmd, err = l.Headless("")
	if err != nil {
		t.Fatal(err)
	}
	args = strings.Join(cmd.Args, " ")
	if strings.Contains(args, "--effort") || strings.Contains(args, "--advisor") || slices.Contains(cmd.Env, SubagentModelEnv+"=haiku") {
		t.Errorf("empty advisor fields changed the launch: %s", args)
	}
}
