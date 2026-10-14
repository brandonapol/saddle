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

// #532: Launch writes the claims plugin into the run dir and the run-dir
// opencode.json loads it.
func TestOpenCodeLaunchWritesClaimsPlugin(t *testing.T) {
	l := Launch{Root: "/repo", Bin: "/opt/saddle bin/saddle", Task: "t9", Dir: t.TempDir(), Prompt: "P", RunDir: t.TempDir()}
	if _, err := (OpenCode{}).Launch(l); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join(l.RunDir, openCodePlugin))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`const bin = "/opt/saddle bin/saddle";`, `"SADDLE_TASK":"t9"`, `"tool.execute.before"`} {
		if !strings.Contains(string(src), want) {
			t.Errorf("plugin lacks %s:\n%s", want, src)
		}
	}
	b, err := os.ReadFile(filepath.Join(l.RunDir, openCodeConfig))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Plugin []string `json:"plugin"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if want := "file://" + filepath.Join(l.RunDir, openCodePlugin); !slices.Equal(cfg.Plugin, []string{want}) {
		t.Errorf("plugin config = %q, want [%q]", cfg.Plugin, want)
	}
}

// #532: the generated plugin, run the way opencode runs plugins (bun, or
// node), asks a fake saddle hook about each write and shell call. A claimed
// path is denied with saddle's reason, an unclaimed one allowed, a read never
// asks, and a broken saddle fails open.
func TestOpenCodeClaimsPluginHarness(t *testing.T) {
	runtime := ""
	for _, r := range []string{"bun", "node"} {
		if _, err := exec.LookPath(r); err == nil {
			runtime = r
			break
		}
	}
	if runtime == "" {
		t.Skip("neither bun nor node installed")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "payloads")
	saddle := filepath.Join(dir, "saddle")
	fake := "#!/usr/bin/env bash\n" +
		"[ \"$1\" = hook ] || exit 2\n" +
		"in=$(cat)\n" +
		"printf '%s %s\\n' \"$SADDLE_TASK\" \"$in\" >> " + shellQuote(log) + "\n" +
		"case $in in *claimed.go*) echo '{\"hookSpecificOutput\":{\"hookEventName\":\"PreToolUse\",\"permissionDecision\":\"deny\",\"permissionDecisionReason\":\"[saddle] claimed.go is claimed by t1\"}}';; esac\n"
	if err := os.WriteFile(saddle, []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	l := Launch{Root: "/repo", Bin: saddle, Task: "t9", Dir: dir, Prompt: "P", RunDir: filepath.Join(dir, "run")}
	if err := os.MkdirAll(l.RunDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := l.writeOpenCodePlugin(); err != nil {
		t.Fatal(err)
	}
	driver := filepath.Join(dir, "driver.mjs")
	if err := os.WriteFile(driver, []byte(`
const { SaddleClaims } = await import(process.argv[2]);
const hooks = await SaddleClaims({ directory: "/wt" });
for (const [tool, args] of JSON.parse(process.argv[3])) {
  try {
    await hooks["tool.execute.before"]({ tool, sessionID: "s", callID: "c" }, { args });
    console.log("allow");
  } catch (e) {
    console.log("deny " + e.message);
  }
}
`), 0o644); err != nil {
		t.Fatal(err)
	}
	drive := func(calls string) []string {
		t.Helper()
		out, err := exec.Command(runtime, driver, filepath.Join(l.RunDir, openCodePlugin), calls).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", runtime, err, out)
		}
		return strings.Split(strings.TrimSpace(string(out)), "\n")
	}

	got := drive(`[["write", {"filePath": "/wt/claimed.go", "content": "x"}],
		["edit", {"filePath": "free.go", "oldString": "a", "newString": "b"}],
		["read", {"filePath": "/wt/claimed.go"}],
		["apply_patch", {"patchText": "*** Begin Patch\n*** Update File: claimed.go\n*** End Patch"}]]`)
	want := []string{"deny [saddle] claimed.go is claimed by t1", "allow", "allow", "deny [saddle] claimed.go is claimed by t1"}
	if !slices.Equal(got, want) {
		t.Fatalf("plugin decisions = %q, want %q", got, want)
	}
	payloads := readLines(t, log)
	if len(payloads) != 3 {
		t.Fatalf("read reached saddle hook, or a call didn't: %q", payloads)
	}
	task, body, _ := strings.Cut(payloads[1], " ")
	var p struct {
		Event string         `json:"hook_event_name"`
		Tool  string         `json:"tool_name"`
		Cwd   string         `json:"cwd"`
		Input map[string]any `json:"tool_input"`
	}
	if err := json.Unmarshal([]byte(body), &p); err != nil {
		t.Fatal(err)
	}
	if task != "t9" || p.Event != "PreToolUse" || p.Tool != "edit" || p.Cwd != "/wt" || p.Input["filePath"] != "free.go" {
		t.Errorf("edit payload = %s %+v", task, p)
	}

	// Fails open: a saddle that errors or can't run lets the call through.
	if err := os.WriteFile(saddle, []byte("#!/bin/sh\necho oops; exit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := drive(`[["write", {"filePath": "/wt/claimed.go"}]]`); !slices.Equal(got, []string{"allow"}) {
		t.Errorf("failing saddle: %q", got)
	}
	if err := os.Remove(saddle); err != nil {
		t.Fatal(err)
	}
	if got := drive(`[["write", {"filePath": "/wt/claimed.go"}]]`); !slices.Equal(got, []string{"allow"}) {
		t.Errorf("missing saddle: %q", got)
	}
}
