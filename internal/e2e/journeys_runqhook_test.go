//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	fa "github.com/brandonapol/saddle/internal/e2e/fakeagent"
)

// TestJourneyHeavyRunsInterceptedInAgentPanes (#240): an agent's heavy
// commands take turns without the agent asking.
//
//  1. In its pane, the flutter shim queues `flutter test` and lets
//     `flutter --version` straight through; the repo's pre-commit hook
//     running `flutter test` is queued too.
//  2. The PreToolUse hook rewrites the agent's `make check` to `saddle run
//     --class go-test --prio worker -- make check`, and the tools the
//     Makefile starts ride that one lease. SADDLE_RUNQ=off leaves it alone.
func TestJourneyHeavyRunsInterceptedInAgentPanes(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not installed")
	}
	w := world(t, Options{})
	must(t, os.MkdirAll(filepath.Join(w.Home, ".config", "saddle"), 0o755))
	must(t, os.WriteFile(filepath.Join(w.Home, ".config", "saddle", "runq.toml"), []byte("mode = \"enforce\"\nmax_load_per_cpu = 0\nmax_cpu_pressure = 0\nheartbeat = \"1s\"\n"), 0o644))
	log := filepath.Join(w.Root, "flutter.log")
	// A fake flutter on the panes' PATH logs its argv and lease.
	must(t, os.WriteFile(filepath.Join(w.Bin, "flutter"),
		[]byte("#!/bin/sh\necho \"flutter $* lease=${SADDLE_RUNQ_LEASE:-none}\" >> "+shq(log)+"\n"), 0o755))
	must(t, os.WriteFile(filepath.Join(w.Repo, ".git", "hooks", "pre-commit"), []byte("#!/bin/sh\nflutter test hook\n"), 0o755))

	w.Spawn("t1", "Flutter work", []string{"lib/**"},
		fa.Run("flutter --version"),
		fa.Run("flutter test widget"),
		fa.Write("lib/a.txt", "a\n"),
		fa.Commit("add a"),
	)
	w.WaitAgentLog("t1", "script finished")
	wt := w.Task("t1").Worktree
	if _, err := os.Stat(filepath.Join(w.Repo, ".saddle", "shims", "flutter")); err != nil {
		t.Fatalf("no flutter shim: %v", err)
	}
	lines := readLines(t, log)
	lease := regexp.MustCompile(`lease=[0-9a-f]{8,}$`)
	if len(lines) != 3 || lines[0] != "flutter --version lease=none" ||
		!strings.HasPrefix(lines[1], "flutter test widget lease=") || !lease.MatchString(lines[1]) ||
		!strings.HasPrefix(lines[2], "flutter test hook lease=") || !lease.MatchString(lines[2]) {
		t.Fatalf("flutter runs in the pane:\n%s\n\nagent log:\n%s", strings.Join(lines, "\n"), w.AgentLog("t1"))
	}

	// The hook, as Claude Code calls it for a Bash tool use.
	hook := func(env []string, cmd string) map[string]any {
		t.Helper()
		in, _ := json.Marshal(map[string]any{
			"session_id": "s1", "hook_event_name": "PreToolUse", "tool_name": "Bash", "permission_mode": "auto",
			"tool_input": map[string]any{"command": cmd, "description": "checks"},
		})
		c := exec.Command(w.Bins.Saddle, "hook")
		c.Dir, c.Env, c.Stdin = wt, append(w.Env(), env...), bytes.NewReader(in)
		out, err := c.Output()
		if err != nil {
			t.Fatalf("saddle hook: %v\n%s", err, out)
		}
		if len(bytes.TrimSpace(out)) == 0 {
			return nil
		}
		var m map[string]any
		must(t, json.Unmarshal(out, &m))
		hs, _ := m["hookSpecificOutput"].(map[string]any)
		return hs
	}
	hs := hook([]string{"SADDLE_TASK=t1"}, "make check")
	in, _ := hs["updatedInput"].(map[string]any)
	rewritten, _ := in["command"].(string)
	if hs["permissionDecision"] != "allow" || rewritten != w.Bins.Saddle+" run --class go-test --prio worker -- make check" || in["description"] != "checks" {
		t.Fatalf("hook output: %v", hs)
	}
	if hs := hook([]string{"SADDLE_TASK=t1", "SADDLE_RUNQ=off"}, "make check"); hs != nil {
		t.Fatalf("SADDLE_RUNQ=off still rewrote: %v", hs)
	}
	if hs := hook([]string{"SADDLE_TASK=t1"}, "flutter --version"); hs != nil {
		t.Fatalf("light command rewritten: %v", hs)
	}

	// Run the rewritten command in the pane's environment: everything the
	// Makefile starts rides its one lease.
	must(t, os.WriteFile(filepath.Join(wt, "Makefile"), []byte("check:\n\tflutter test inner\n\tflutter analyze\n"), 0o644))
	must(t, os.Remove(log))
	pane := append(w.Env(), "SADDLE_TASK=t1", "SADDLE_ROOT="+w.Repo,
		"PATH="+filepath.Join(w.Repo, ".saddle", "shims")+string(os.PathListSeparator)+filepath.Dir(w.Bins.Saddle)+string(os.PathListSeparator)+envPath(w.Env()))
	c := exec.Command("bash", "-c", rewritten)
	c.Dir, c.Env = wt, pane
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", rewritten, err, out)
	}
	lines = readLines(t, log)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "flutter test inner lease=") || !lease.MatchString(lines[0]) ||
		lines[1] != "flutter analyze "+strings.TrimPrefix(lines[0], "flutter test inner ") {
		t.Fatalf("tools under make check should ride its one lease:\n%s", strings.Join(lines, "\n"))
	}

	// The runs that queued are in the repo's activity log; the light ones
	// and the nested ones aren't.
	evs, err := w.App().Store.Events(500)
	must(t, err)
	var finished []string
	for _, e := range evs {
		if e.Kind == "run_finished" {
			finished = append(finished, e.Data)
		}
	}
	if len(finished) != 3 || strings.Count(strings.Join(finished, "\n"), "flutter-test ok") != 2 || strings.Count(strings.Join(finished, "\n"), "go-test ok") != 1 {
		t.Fatalf("run_finished events:\n%s", strings.Join(finished, "\n"))
	}
}

func readLines(t *testing.T, p string) []string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func envPath(env []string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if v, ok := strings.CutPrefix(env[i], "PATH="); ok {
			return v
		}
	}
	return ""
}
