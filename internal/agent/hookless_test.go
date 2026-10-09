package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func launchScript(t *testing.T, a Adapter, l Launch) (cmd, script, prompt string) {
	t.Helper()
	cmd, err := a.Launch(l)
	if err != nil {
		t.Fatal(err)
	}
	s, err := os.ReadFile(filepath.Join(l.RunDir, "launch.sh"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := os.ReadFile(filepath.Join(l.RunDir, "prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	return cmd, string(s), string(p)
}

func TestCodexLaunchWiresMCPAndBrief(t *testing.T) {
	l := Launch{Root: "/repo", Bin: "/bin/saddle", Task: "t5", Title: "x", Dir: "/repo/wt", Model: "gpt-5-codex",
		Brief: "BRIEF", Prompt: "PROMPT", RunDir: t.TempDir(), Args: []string{"--sandbox", "danger-full-access"}}
	a, err := ByName("codex")
	if err != nil {
		t.Fatal(err)
	}
	if a.Hooks() {
		t.Fatal("codex has no saddle hooks")
	}
	cmd, script, prompt := launchScript(t, a, l)
	if !strings.HasPrefix(cmd, "bash ") {
		t.Errorf("cmd = %q", cmd)
	}
	for _, want := range []string{"'codex'", "--model 'gpt-5-codex'", `mcp_servers.saddle.command="/bin/saddle"`,
		`mcp_servers.saddle.args=["mcp"]`, `SADDLE_TASK="t5"`, "'--sandbox' 'danger-full-access'",
		"cd '/repo/wt'", `"$(cat "$run/prompt.md")"`, "'/bin/saddle' exited 't5'"} {
		if !strings.Contains(script, want) {
			t.Errorf("launch.sh lacks %s:\n%s", want, script)
		}
	}
	// No hooks: the brief rides in the first message, with the advisory claim rules.
	if !strings.HasPrefix(prompt, "BRIEF") || !strings.HasSuffix(prompt, "PROMPT") || !strings.Contains(prompt, "advisory") {
		t.Errorf("prompt.md = %q", prompt)
	}
	if got := Recorded(l.RunDir); got != "codex" {
		t.Errorf("recorded adapter %q", got)
	}
}

// #181: a grok spawn, with no grok harness configured, used to launch
// --directory and --prompt, which the grok CLI rejects, and attached no
// saddle MCP server. Every grok worker now runs the full harness.
func TestGrokAdapterLaunchesTheHarness(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	l := Launch{Root: dir, Bin: "/bin/saddle", Task: "t6", Title: "meter", Dir: dir, Model: "grok-4",
		Brief: "BRIEF", Prompt: "fix it", RunDir: t.TempDir()}
	a, err := ByName("grok")
	if err != nil {
		t.Fatal(err)
	}
	if !a.Hooks() {
		t.Error("a grok worker runs saddle hook through its project hooks")
	}
	if got := a.Inject("1. [action] rebase"); got != WakeLine {
		t.Errorf("grok gets notices from its hook, so inject only wakes it: %q", got)
	}
	_, script, _ := launchScript(t, a, l)
	for _, bad := range []string{"--directory", "--prompt", "tee -a"} {
		if strings.Contains(script, bad) {
			t.Errorf("launch.sh uses %s, which grok rejects or which ends the session:\n%s", bad, script)
		}
	}
	for _, want := range []string{"cd '" + dir + "'", "'--model' 'grok-4'", "'--permission-mode' 'bypassPermissions'", `args+=(-- "$(cat "$run/prompt.md")")`} {
		if !strings.Contains(script, want) {
			t.Errorf("launch.sh lacks %s:\n%s", want, script)
		}
	}
	mcp, err := os.ReadFile(filepath.Join(dir, ".grok", "config.toml"))
	if err != nil || !strings.Contains(string(mcp), "[mcp_servers.saddle]") {
		t.Errorf("no saddle MCP server for the worker: %s %v", mcp, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".grok", "hooks", "saddle.json")); err != nil {
		t.Errorf("no saddle hooks: %v", err)
	}
	if Recorded(l.RunDir) != "grok" || !GrokHarnessOn(l.RunDir) {
		t.Error("grok worker not recorded as the harness")
	}
}

func TestHooklessInjectTypesNoticesOnOneLine(t *testing.T) {
	a, _ := ByName("codex")
	got := a.Inject("1. [action] rebase\n2. [info] t3 landed\n")
	if strings.Contains(got, "\n") || !strings.Contains(got, "rebase") || !strings.Contains(got, "t3 landed") || !strings.HasPrefix(got, "[saddle]") {
		t.Errorf("inject = %q", got)
	}
}

func TestRecordedDefaultsToClaude(t *testing.T) {
	if got := Recorded(t.TempDir()); got != "claude" {
		t.Errorf("got %q", got)
	}
}

func writeRollout(t *testing.T, path, cwd string, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"timestamp":"2026-10-02T10:00:00Z","type":"session_meta","payload":{"id":"x","cwd":"` + cwd + `"}}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

func TestCodexTranscriptIsNewestRolloutForWorktree(t *testing.T) {
	home := t.TempDir()
	c := Codex{Home: home}
	if got := c.Usage().Transcript("/repo/wt", "", ""); got != "" {
		t.Errorf("no rollouts yet: %q", got)
	}
	now := time.Now()
	day := filepath.Join(home, "sessions", "2026", "10", "02")
	old := filepath.Join(day, "rollout-a.jsonl")
	mine := filepath.Join(day, "rollout-b.jsonl")
	writeRollout(t, old, "/repo/wt", now.Add(-time.Hour))
	writeRollout(t, mine, "/repo/wt", now)
	writeRollout(t, filepath.Join(day, "rollout-c.jsonl"), "/repo/other", now.Add(time.Minute))
	if got := c.Usage().Transcript("/repo/wt", "", ""); got != mine {
		t.Errorf("got %q want %q", got, mine)
	}
}

// #182: gemini launches interactively in the worktree with the saddle MCP
// server from a run-dir system settings file, so nothing lands in the repo.
func TestGeminiLaunchWiresMCPAndBrief(t *testing.T) {
	l := Launch{Root: "/repo", Bin: "/bin/saddle", Task: "t7", Title: "x", Dir: "/repo/wt", Model: "gemini-2.5-pro",
		Mode: "acceptEdits", Brief: "BRIEF", Prompt: "PROMPT", RunDir: t.TempDir(), Args: []string{"--sandbox"}}
	a, err := ByName("gemini")
	if err != nil {
		t.Fatal(err)
	}
	if a.Hooks() {
		t.Fatal("gemini runs no saddle hooks")
	}
	_, script, prompt := launchScript(t, a, l)
	for _, want := range []string{"'gemini'", "--model 'gemini-2.5-pro'", "--approval-mode 'auto_edit'", "--skip-trust",
		"'--sandbox'", `-i "$(cat "$run/prompt.md")"`, `export GEMINI_CLI_SYSTEM_SETTINGS_PATH="$run/gemini-settings.json"`,
		"cd '/repo/wt'", "'/bin/saddle' exited 't7'"} {
		if !strings.Contains(script, want) {
			t.Errorf("launch.sh lacks %s:\n%s", want, script)
		}
	}
	if !strings.HasPrefix(prompt, "BRIEF") || !strings.HasSuffix(prompt, "PROMPT") || !strings.Contains(prompt, "advisory") {
		t.Errorf("prompt.md = %q", prompt)
	}
	b, err := os.ReadFile(filepath.Join(l.RunDir, "gemini-settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &settings); err != nil {
		t.Fatal(err)
	}
	s := settings.MCPServers["saddle"]
	if s.Command != "/bin/saddle" || len(s.Args) != 1 || s.Args[0] != "mcp" || s.Env["SADDLE_TASK"] != "t7" || s.Env["SADDLE_ROOT"] != "/repo" {
		t.Errorf("saddle MCP server = %+v", s)
	}
	if Recorded(l.RunDir) != "gemini" {
		t.Error("gemini not recorded")
	}
}

func TestGeminiApprovalMode(t *testing.T) {
	for mode, want := range map[string]string{"": "yolo", "bypassPermissions": "yolo", "acceptEdits": "auto_edit", "plan": "plan", "default": "default"} {
		if got := geminiApproval(mode); got != want {
			t.Errorf("geminiApproval(%q) = %q, want %q", mode, got, want)
		}
	}
}
