package agent

import (
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

func TestGrokLaunchIsHeadlessAndTeesUsage(t *testing.T) {
	l := Launch{Root: "/repo", Bin: "/bin/saddle", Task: "t6", Title: "hero art", Dir: "/repo/wt", Model: "grok-4",
		Brief: "BRIEF", Prompt: "draw", RunDir: t.TempDir()}
	a, err := ByName("grok")
	if err != nil {
		t.Fatal(err)
	}
	_, script, prompt := launchScript(t, a, l)
	for _, want := range []string{"'grok'", "--model 'grok-4'", "--directory '/repo/wt'", `--prompt "$(cat "$run/prompt.md")"`,
		`tee -a "$run/transcript.jsonl"`} {
		if !strings.Contains(script, want) {
			t.Errorf("launch.sh lacks %s:\n%s", want, script)
		}
	}
	// Grok has no MCP server from saddle: it finishes through the CLI.
	if !strings.Contains(prompt, "saddle done") {
		t.Errorf("prompt.md lacks the CLI done step: %q", prompt)
	}
	if got := a.Usage().Transcript(l.Dir, l.RunDir, ""); got != filepath.Join(l.RunDir, "transcript.jsonl") {
		t.Errorf("transcript %q", got)
	}
	if Recorded(l.RunDir) != "grok" {
		t.Error("grok not recorded")
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
