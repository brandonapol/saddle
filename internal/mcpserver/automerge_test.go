package mcpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/brandonapol/saddle/internal/store"
)

// noGH puts a gh on PATH that always fails, so no test reaches GitHub.
func noGH(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// #152: the automerge tool turns auto-merge on and off, reports it, and
// holds and releases a stack.
func TestAutomergeTool(t *testing.T) {
	a, _ := stackSetup(t)
	noGH(t)
	var out automerge.Status
	if res := callTool(t, a, app.OrchestratorID, "automerge", map[string]any{"action": "status"}, &out); res.IsError || out.Enabled {
		t.Fatalf("status: %s %+v", errText(res), out)
	}
	if res := callTool(t, a, app.OrchestratorID, "automerge", map[string]any{"action": "on"}, &out); res.IsError || !out.Enabled || out.Source != automerge.SourceRuntime {
		t.Fatalf("on: %s %+v", errText(res), out)
	}
	if res := callTool(t, a, app.OrchestratorID, "automerge", map[string]any{"action": "hold", "stack": "t1"}, &out); res.IsError || len(out.Holds) != 1 || out.Holds[0] != "t1" {
		t.Fatalf("hold: %s %+v", errText(res), out)
	}
	if res := callTool(t, a, app.OrchestratorID, "automerge", map[string]any{"action": "hold"}, nil); !res.IsError {
		t.Fatal("hold without a stack succeeded")
	}
	out = automerge.Status{}
	if res := callTool(t, a, app.OrchestratorID, "automerge", map[string]any{"action": "release", "stack": "t1"}, &out); res.IsError || len(out.Holds) != 0 {
		t.Fatalf("release: %s %+v", errText(res), out)
	}
	if res := callTool(t, a, app.OrchestratorID, "automerge", map[string]any{"action": "off"}, &out); res.IsError || out.Enabled {
		t.Fatalf("off: %s %+v", errText(res), out)
	}
	if res := callTool(t, a, app.OrchestratorID, "automerge", map[string]any{"action": "merge-everything"}, nil); !res.IsError {
		t.Fatal("unknown action succeeded")
	}
}

// queueTwo queues t2 and t3 behind the landed t1.
func queueTwo(t *testing.T, a *app.App) {
	t.Helper()
	for _, id := range []string{"t2", "t3"} {
		tk, err := a.Spawn(app.SpawnReq{ID: id, Title: id})
		if err != nil {
			t.Fatal(err)
		}
		write(t, tk.Worktree, id+".txt", id+"\n")
		t.Setenv("SADDLE_TASK", id)
		git(t, tk.Worktree, "add", "-A")
		git(t, tk.Worktree, "commit", "-qm", id)
		t.Setenv("SADDLE_TASK", "")
		if err := a.Done(id, id); err != nil {
			t.Fatal(err)
		}
	}
}

func queueOrder(t *testing.T, a *app.App) string {
	t.Helper()
	q, err := a.Queue()
	if err != nil {
		t.Fatal(err)
	}
	var s []string
	for _, e := range q {
		s = append(s, e.Task+":"+e.State)
	}
	return strings.Join(s, " ")
}

// #146: queue_move, queue_hold and queue_release steer the merge train.
func TestQueueTools(t *testing.T) {
	a, _ := stackSetup(t)
	queueTwo(t, a)
	if got := queueOrder(t, a); got != "t2:queued t3:queued" {
		t.Fatalf("queue = %s", got)
	}
	if res := callTool(t, a, app.OrchestratorID, "queue_move", map[string]any{"task": "t3", "position": 1}, nil); res.IsError {
		t.Fatalf("queue_move: %s", errText(res))
	}
	if got := queueOrder(t, a); got != "t3:queued t2:queued" {
		t.Fatalf("after move = %s", got)
	}
	if res := callTool(t, a, app.OrchestratorID, "queue_hold", map[string]any{"task": "t3", "reason": "owner wants to look"}, nil); res.IsError {
		t.Fatalf("queue_hold: %s", errText(res))
	}
	if got := queueOrder(t, a); got != "t3:"+store.OnHold+" t2:queued" {
		t.Fatalf("after hold = %s", got)
	}
	if res := callTool(t, a, app.OrchestratorID, "queue_release", map[string]any{"task": "t3"}, nil); res.IsError {
		t.Fatalf("queue_release: %s", errText(res))
	}
	if got := queueOrder(t, a); got != "t3:queued t2:queued" {
		t.Fatalf("after release = %s", got)
	}
}

func TestQueueToolsErrors(t *testing.T) {
	a, _ := stackSetup(t)
	queueTwo(t, a)
	for _, c := range []struct {
		name string
		args map[string]any
	}{
		{"queue_move", map[string]any{"task": "t9", "position": 1}},
		{"queue_hold", map[string]any{"task": "t1"}},    // landed, not waiting
		{"queue_release", map[string]any{"task": "t2"}}, // not held
	} {
		if res := callTool(t, a, app.OrchestratorID, c.name, c.args, nil); !res.IsError {
			t.Errorf("%s %v succeeded", c.name, c.args)
		}
	}
}
