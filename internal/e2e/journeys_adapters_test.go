//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/mcpserver"
)

// TestJourneyAdapters (#181, #182): the orchestrator spawns a grok worker
// from a claude session. It starts the grok harness in the worktree, with
// the saddle MCP server and no flags grok rejects. Status lists the
// adapters, and a gemini spawn that can't sign in fails with the reason
// and creates no task.
func TestJourneyAdapters(t *testing.T) {
	for _, k := range []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GENAI_USE_GCA"} {
		if os.Getenv(k) != "" {
			t.Skip(k + " is set, so gemini counts as signed in")
		}
	}
	w := world(t, Options{})
	args := filepath.Join(w.Root, "grok-args")
	fake := "#!/usr/bin/env bash\nprintf '%s\\n' \"$@\" > " + args + "\nexec sleep 600\n"
	must(t, os.WriteFile(filepath.Join(w.Bin, "grok"), []byte(fake), 0o755))
	must(t, os.WriteFile(filepath.Join(w.Bin, "gemini"), []byte("#!/bin/sh\nexit 0\n"), 0o755))
	must(t, os.MkdirAll(filepath.Join(w.Home, ".grok"), 0o755))
	must(t, os.WriteFile(filepath.Join(w.Home, ".grok", "auth.json"), []byte("{}"), 0o600))

	st := w.Status()
	avail := map[string]bool{}
	for _, s := range st.Adapters {
		avail[s.Name] = s.OK
	}
	if !avail["grok"] || avail["gemini"] {
		t.Fatalf("adapters = %+v", st.Adapters)
	}

	out, isErr := w.MCP("t0", "spawn", map[string]any{"title": "Gemini work", "prompt": "x", "adapter": "gemini"})
	if !isErr || !strings.Contains(out, "adapter gemini is not available") || !strings.Contains(out, "not logged in") {
		t.Fatalf("gemini spawn: err=%v %s", isErr, out)
	}
	if n := len(w.Status().Tasks); n != len(st.Tasks) {
		t.Fatalf("an unavailable adapter left a task behind: %d tasks, had %d", n, len(st.Tasks))
	}

	if out, isErr := w.MCP("t0", "spawn", map[string]any{"title": "Grok work", "prompt": "fix the meter", "adapter": "grok"}); isErr {
		t.Fatalf("grok spawn: %s", out)
	}
	var task mcpserver.TaskView
	for _, v := range w.Status().Tasks {
		if v.Title == "Grok work" {
			task = v
		}
	}
	if task.ID == "" {
		t.Fatal("no grok task")
	}
	got := waitFile(t, args)
	for _, bad := range []string{"--directory", "--prompt"} {
		if strings.Contains(got, bad+"\n") {
			t.Errorf("grok launched with %s, which it rejects:\n%s", bad, got)
		}
	}
	for _, want := range []string{"--permission-mode\n", "--rules\n", "--\nfix the meter"} {
		if !strings.Contains(got, want) {
			t.Errorf("grok args lack %q:\n%s", want, got)
		}
	}
	cfg, err := os.ReadFile(filepath.Join(task.Worktree, ".grok", "config.toml"))
	if err != nil || !strings.Contains(string(cfg), "[mcp_servers.saddle]") {
		t.Errorf("grok worker has no saddle MCP server: %s %v", cfg, err)
	}
}

// waitFile waits for path to have content and returns it.
func waitFile(t *testing.T, path string) string {
	t.Helper()
	var body string
	Eventually(t, "write "+filepath.Base(path), func() error {
		b, err := os.ReadFile(path)
		if err != nil || len(b) == 0 {
			return errorf("%s not written yet", path)
		}
		body = string(b)
		return nil
	})
	return body
}
