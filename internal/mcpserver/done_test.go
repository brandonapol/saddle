package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
)

// #368: the done tool answers while a slow gate still runs, and a retry
// (Claude Code giving up and calling again) joins the job instead of
// starting a second check.
func TestDoneToolReturnsWhileGateRuns(t *testing.T) {
	a, _ := stackSetup(t)
	dir := t.TempDir()
	runs, rel := filepath.Join(dir, "runs"), filepath.Join(dir, "release")
	release := func() { _ = os.WriteFile(rel, nil, 0o644) }
	t.Cleanup(func() { release(); a.WaitDoneJobs() })
	a.Cfg.Train.Lint = config.Lint{Cmd: "echo run >> '" + runs + "'; while [ ! -e '" + rel + "' ]; do sleep 0.05; done", Set: true}
	tk, err := a.Spawn(app.SpawnReq{ID: "t2", Title: "slow"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, tk.Worktree, "t2.txt", "t2\n")
	t.Setenv("SADDLE_TASK", "t2")
	git(t, tk.Worktree, "add", "-A")
	git(t, tk.Worktree, "commit", "-qm", "t2")
	t.Setenv("SADDLE_TASK", "")

	start := time.Now()
	var first, second OK
	if res := callTool(t, a, "t2", "done", map[string]any{"summary": "slow"}, &first); res.IsError {
		t.Fatalf("done: %s", errText(res))
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("done held the call for %s", time.Since(start))
	}
	if !strings.Contains(first.Message, "in the background") {
		t.Fatalf("first done: %q", first.Message)
	}
	if res := callTool(t, a, "t2", "done", map[string]any{"summary": "slow"}, &second); res.IsError {
		t.Fatalf("retry: %s", errText(res))
	}
	if !strings.Contains(second.Message, "already checking") {
		t.Fatalf("retry: %q", second.Message)
	}
	release()
	a.WaitDoneJobs()
	if b, _ := os.ReadFile(runs); strings.Count(string(b), "run") != 1 {
		t.Fatalf("gate ran %q", b)
	}
	if got := queueOrder(t, a); got != "t2:queued" {
		t.Fatalf("queue = %q", got)
	}
}
