package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGrokWorkerLaunch(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	run := t.TempDir()
	l := Launch{
		Kind: KindGrok, Bin: "/usr/bin/saddle", Task: "t3", Title: "meter",
		Dir: dir, Cmd: "grok", Model: "grok-4.5", Mode: "bypassPermissions",
		Brief: "stay in your worktree", Prompt: "fix the meter", RunDir: run,
		ExtraEnv: map[string]string{"SADDLE_ROOT": dir},
	}
	// env() only copies ExtraEnv plus SADDLE_ROOT from Root and SADDLE_TASK.
	l.Root = dir
	sh, err := l.Write()
	if err != nil {
		t.Fatal(err)
	}
	script, err := os.ReadFile(strings.Trim(strings.TrimPrefix(sh, "bash "), "'"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	for _, want := range []string{"--trust", "--no-alt-screen", "--permission-mode", "bypassPermissions", "--model", "grok-4.5", "stay in your worktree", "SADDLE_TASK"} {
		if !strings.Contains(text, want) {
			t.Errorf("launch.sh missing %q\n%s", want, text)
		}
	}
	if strings.Contains(text, "--settings") || strings.Contains(text, "claude") {
		t.Fatalf("grok launch still looks like claude:\n%s", text)
	}
	hook, err := os.ReadFile(filepath.Join(dir, ".grok", "hooks", "saddle.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(hook), "saddle' hook") && !strings.Contains(string(hook), "saddle hook") {
		t.Fatalf("hook file: %s", hook)
	}
	cfg, err := os.ReadFile(filepath.Join(dir, ".grok", "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "[mcp_servers.saddle]") || !strings.Contains(string(cfg), "t3") {
		t.Fatalf("mcp config: %s", cfg)
	}
	out, err := exec.Command("git", "-C", dir, "status", "--porcelain").Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("grok files are visible to git:\n%s", out)
	}
}

func TestGrokOrchestratorCannotWrite(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	l := Launch{
		Kind: KindGrok, Root: dir, Bin: "/bin/saddle", Task: "t0", Title: "orchestrator",
		Dir: dir, Cmd: "grok", RunDir: t.TempDir(), Deny: OrchestratorDeny(), Brief: "do not edit",
	}
	cmd, err := l.Headless("old-session")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(cmd.Args, " "), "grok-bridge") {
		t.Fatalf("args = %v", cmd.Args)
	}
	var spec grokSpec
	b, err := os.ReadFile(filepath.Join(l.RunDir, "grok.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Resume != "old-session" {
		t.Fatalf("resume = %q", spec.Resume)
	}
	joined := strings.Join(spec.Args, " ")
	for _, want := range []string{"--no-subagents", "--disallowed-tools", "write_file", "search_replace", "apply_patch", "write,", "spawn_subagent", "scheduler_create", "monitor", "streaming-messages-json", "--include-partial-messages", "Bash(git merge:*)", "Bash(git push:*)"} {
		if !strings.Contains(joined, want) {
			t.Errorf("grok args missing %q\n%s", want, joined)
		}
	}
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v %s", args, err, out)
		}
	}
	c := exec.Command("git", "-C", dir, "commit", "-qm", "init", "--allow-empty")
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
}
