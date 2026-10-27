//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/mcpserver"
)

// TestJourneyOpenCodeWorker (#508): with a fake opencode on PATH and a login,
// status lists opencode as available, an orchestrator spawn on it starts the
// TUI in the worktree with the prompt, an approval flag for the permission
// mode and a run-dir OPENCODE_CONFIG that carries the saddle MCP server and
// the claims plugin, and nothing lands in the repo. Without a login the spawn
// is refused by name and leaves no task. No real opencode or account is used.
func TestJourneyOpenCodeWorker(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{})
	data := filepath.Join(w.Home, ".local", "share")
	t.Setenv("XDG_DATA_HOME", data)
	for _, k := range []string{"OPENCODE_API_KEY", "OPENCODE_AUTH_CONTENT", "ANTHROPIC_API_KEY", "OPENAI_API_KEY", "OPENROUTER_API_KEY"} {
		t.Setenv(k, "")
	}
	args := filepath.Join(w.Root, "opencode-args")
	cfgCopy := filepath.Join(w.Root, "opencode-config")
	fake := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" > " + args + "\ncp \"$OPENCODE_CONFIG\" " + cfgCopy + "\nexec sleep 600\n"
	must(t, os.WriteFile(filepath.Join(w.Bin, "opencode"), []byte(fake), 0o755))

	// Not logged in: refused by name, no task.
	before := len(w.Status().Tasks)
	out, isErr := w.MCP("t0", "spawn", map[string]any{"title": "OC early", "prompt": "x", "adapter": "opencode"})
	if !isErr || !strings.Contains(out, "adapter opencode is not available") || !strings.Contains(out, "opencode providers login") {
		t.Fatalf("logged-out opencode spawn: err=%v %s", isErr, out)
	}
	if n := len(w.Status().Tasks); n != before {
		t.Fatalf("an unavailable adapter left a task behind: %d tasks, had %d", n, before)
	}

	must(t, os.MkdirAll(filepath.Join(data, "opencode"), 0o755))
	must(t, os.WriteFile(filepath.Join(data, "opencode", "auth.json"), []byte("{}"), 0o600))
	ok := false
	for _, s := range w.Status().Adapters {
		if s.Name == "opencode" {
			ok = s.OK
		}
	}
	if !ok {
		t.Fatalf("opencode not available after login: %+v", w.Status().Adapters)
	}

	if out, isErr := w.MCP("t0", "spawn", map[string]any{"title": "OC work", "prompt": "fix the meter", "adapter": "opencode"}); isErr {
		t.Fatalf("opencode spawn: %s", out)
	}
	var task mcpserver.TaskView
	for _, v := range w.Status().Tasks {
		if v.Title == "OC work" {
			task = v
		}
	}
	if task.ID == "" {
		t.Fatal("no opencode task")
	}
	if task.Adapter != "opencode" {
		t.Errorf("task adapter = %q, want opencode", task.Adapter)
	}
	got := waitFile(t, args)
	if !strings.Contains(got, "--prompt\n") || !strings.Contains(got, "fix the meter") {
		t.Errorf("opencode args lack the prompt:\n%s", got)
	}
	cfg := waitFile(t, cfgCopy)
	for _, want := range []string{`"saddle"`, `"local"`, "saddle-claims.mjs"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("OPENCODE_CONFIG lacks %q:\n%s", want, cfg)
		}
	}
	if _, err := os.Stat(filepath.Join(task.Worktree, "opencode.json")); err == nil {
		t.Error("opencode.json was written into the worktree; it belongs in the run dir")
	}
}
