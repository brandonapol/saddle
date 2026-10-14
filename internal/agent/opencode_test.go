package agent

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// #509: launch.sh runs opencode's TUI in the worktree with the model, the
// unattended flag, [adapters.opencode] args and the first message as
// --prompt, and points OPENCODE_CONFIG at a run-dir config naming the saddle
// MCP server. A fake opencode records what it was given.
func TestOpenCodeLaunchRunsFakeBinary(t *testing.T) {
	bin, wt, out := t.TempDir(), t.TempDir(), t.TempDir()
	fake := "#!/usr/bin/env bash\n" +
		"printf '%s\\n' \"$@\" > " + shellQuote(filepath.Join(out, "args")) + "\n" +
		"pwd > " + shellQuote(filepath.Join(out, "pwd")) + "\n" +
		"printf '%s\\n' \"$OPENCODE_CONFIG\" \"$SADDLE_HOOKLESS\" \"$OPENCODE_DISABLE_AUTOUPDATE\" > " + shellQuote(filepath.Join(out, "env")) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "opencode"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	saddle := filepath.Join(bin, "saddle")
	if err := os.WriteFile(saddle, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	l := Launch{Root: "/repo", Bin: saddle, Task: "t9", Title: "x", Dir: wt, Model: "anthropic/claude-sonnet-4-5",
		Brief: "BRIEF", Prompt: "PROMPT", RunDir: t.TempDir(), Args: []string{"--agent", "build"}}
	a, err := ByName("opencode")
	if err != nil {
		t.Fatal(err)
	}
	if a.Hooks() {
		t.Fatal("opencode's claims plugin is not a full hook set: Hooks() stays false (#532)")
	}
	cmd, err := a.Launch(l)
	if err != nil {
		t.Fatal(err)
	}
	run := exec.Command("bash", "-c", cmd)
	run.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "SHELL=true")
	if b, err := run.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", cmd, err, b)
	}

	args := readLines(t, filepath.Join(out, "args"))
	prompt, _ := os.ReadFile(filepath.Join(l.RunDir, "prompt.md"))
	want := []string{"--model", "anthropic/claude-sonnet-4-5", "--auto", "--agent", "build", "--prompt", string(prompt)}
	if !slices.Equal(args, strings.Split(strings.Join(want, "\n"), "\n")) {
		t.Errorf("opencode args = %q, want %q", args, want)
	}
	if !strings.HasPrefix(string(prompt), "BRIEF") || !strings.HasSuffix(string(prompt), "PROMPT") || !strings.Contains(string(prompt), "checks claims when you commit") {
		t.Errorf("prompt.md = %q", prompt)
	}
	if got := readLines(t, filepath.Join(out, "pwd")); got[0] != wt {
		t.Errorf("opencode ran in %s, want %s", got[0], wt)
	}
	env := readLines(t, filepath.Join(out, "env"))
	cfgPath := filepath.Join(l.RunDir, "opencode.json")
	if env[0] != cfgPath || env[1] != "1" || env[2] != "1" {
		t.Errorf("OPENCODE_CONFIG, SADDLE_HOOKLESS, OPENCODE_DISABLE_AUTOUPDATE = %q", env)
	}

	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCP map[string]struct {
			Type        string            `json:"type"`
			Command     []string          `json:"command"`
			Environment map[string]string `json:"environment"`
			Enabled     bool              `json:"enabled"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	s := cfg.MCP["saddle"]
	if s.Type != "local" || !slices.Equal(s.Command, []string{saddle, "mcp"}) || !s.Enabled ||
		s.Environment["SADDLE_TASK"] != "t9" || s.Environment["SADDLE_ROOT"] != "/repo" {
		t.Errorf("saddle MCP server = %+v", s)
	}
	if Recorded(l.RunDir) != "opencode" {
		t.Error("opencode not recorded")
	}
}

func TestOpenCodeLaunchWithoutModel(t *testing.T) {
	l := Launch{Root: "/repo", Bin: "/bin/saddle", Task: "t9", Dir: "/repo/wt", Mode: "plan", Prompt: "P", RunDir: t.TempDir(), Cmd: "my-opencode"}
	_, script, _ := launchScript(t, OpenCode{}, l)
	if strings.Contains(script, "--model") || strings.Contains(script, "--auto") {
		t.Errorf("no model and plan mode, yet:\n%s", script)
	}
	for _, want := range []string{"'my-opencode' '--agent' 'plan'", `--prompt "$(cat "$run/prompt.md")"`} {
		if !strings.Contains(script, want) {
			t.Errorf("launch.sh lacks %s:\n%s", want, script)
		}
	}
}

func TestOpenCodeApprovalFlags(t *testing.T) {
	for mode, want := range map[string][]string{
		"": {"--auto"}, "bypassPermissions": {"--auto"}, "acceptEdits": nil, "default": nil, "plan": {"--agent", "plan"},
	} {
		if got := openCodeApproval(mode); !slices.Equal(got, want) {
			t.Errorf("openCodeApproval(%q) = %q, want %q", mode, got, want)
		}
	}
}

// #509: status lists opencode, and says why it can't run: not installed,
// or installed without a provider login.
func TestOpenCodeAvailability(t *testing.T) {
	m := fakeMachine(t, nil, nil)
	if err := m.check("opencode", ""); err == nil || !strings.Contains(err.Error(), "opencode not on PATH") {
		t.Errorf("not installed: %v", err)
	}
	m = fakeMachine(t, []string{"opencode"}, nil)
	var listed bool
	for _, s := range m.availability(nil) {
		if s.Name == "opencode" {
			listed = true
			if s.OK || s.Hooks || !strings.Contains(s.Reason, "opencode providers login") {
				t.Errorf("opencode without credentials: %+v", s)
			}
		}
	}
	if !listed {
		t.Fatal("availability does not list opencode")
	}
	touch(t, filepath.Join(m.home, ".local", "share", "opencode", "auth.json"))
	if err := m.check("opencode", ""); err != nil {
		t.Errorf("logged in under ~/.local/share: %v", err)
	}

	xdg := t.TempDir()
	m = fakeMachine(t, []string{"opencode"}, map[string]string{"XDG_DATA_HOME": xdg})
	if err := m.check("opencode", ""); err == nil {
		t.Error("no auth.json under $XDG_DATA_HOME/opencode")
	}
	touch(t, filepath.Join(xdg, "opencode", "auth.json"))
	if err := m.check("opencode", ""); err != nil {
		t.Errorf("logged in under $XDG_DATA_HOME: %v", err)
	}

	m = fakeMachine(t, []string{"opencode"}, map[string]string{"OPENROUTER_API_KEY": "k"})
	if err := m.check("opencode", ""); err != nil {
		t.Errorf("a provider key in the environment: %v", err)
	}
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}
