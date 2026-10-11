package mcpserver

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

// orchSetup is stackSetup (t1 landed, README.md changed) plus three live
// tasks with claims and one killed task.
func orchSetup(t *testing.T) *app.App {
	t.Helper()
	a, _ := stackSetup(t)
	for _, r := range []app.SpawnReq{
		{ID: "t2", Title: "api", Claims: []string{"api/**"}, Force: true},
		{ID: "t3", Title: "docs", Claims: []string{"docs/**"}, Force: true},
		{ID: "t4", Title: "web", Claims: []string{"web/**"}, Force: true},
		{ID: "t5", Title: "gone", Claims: []string{"gone/**"}, Force: true},
	} {
		if _, err := a.Spawn(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Kill("t5", false); err != nil {
		t.Fatal(err)
	}
	return a
}

func taskIDs(ts []TaskView) []string {
	var ids []string
	for _, v := range ts {
		ids = append(ids, v.ID)
	}
	return ids
}

// #340: status with no arguments is compact: one short line of fields per
// task, no claims, worktrees or windows, and finished (killed) tasks are
// counted rather than listed. detail=true is the full dump.
func TestStatusToolCompactByDefault(t *testing.T) {
	a := orchSetup(t)
	var compact StatusOut
	res := callTool(t, a, app.OrchestratorID, "status", map[string]any{}, &compact)
	if res.IsError {
		t.Fatalf("status: %s", errText(res))
	}
	if slices.Contains(taskIDs(compact.Tasks), "t5") {
		t.Errorf("compact status lists the killed task: %v", taskIDs(compact.Tasks))
	}
	if compact.Hidden["killed"] != 1 || !strings.Contains(compact.Hint, "detail") {
		t.Errorf("hidden = %v, hint = %q", compact.Hidden, compact.Hint)
	}
	for _, v := range compact.Tasks {
		if len(v.Claims) > 0 || v.Worktree != "" || v.Window != "" || v.Branch != "" {
			t.Errorf("compact task carries detail: %+v", v)
		}
		if v.ID == "" || v.Status == "" || v.Title == "" {
			t.Errorf("compact task lost its essentials: %+v", v)
		}
	}

	var full StatusOut
	if res := callTool(t, a, app.OrchestratorID, "status", map[string]any{"detail": true}, &full); res.IsError {
		t.Fatalf("status detail: %s", errText(res))
	}
	if !slices.Contains(taskIDs(full.Tasks), "t5") || len(full.Hidden) != 0 {
		t.Errorf("detail tasks = %v, hidden = %v", taskIDs(full.Tasks), full.Hidden)
	}
	claimed := false
	for _, v := range full.Tasks {
		claimed = claimed || len(v.Claims) > 0
	}
	if !claimed {
		t.Error("detail status lost the claims")
	}
	cb, _ := json.Marshal(compact)
	fb, _ := json.Marshal(full)
	if len(cb) >= len(fb) {
		t.Errorf("compact (%d bytes) is not smaller than detail (%d bytes)", len(cb), len(fb))
	}
}

// #340: status filters by state, ids and since.
func TestStatusToolFilters(t *testing.T) {
	a := orchSetup(t)
	get := func(args map[string]any) StatusOut {
		t.Helper()
		var out StatusOut
		if res := callTool(t, a, app.OrchestratorID, "status", args, &out); res.IsError {
			t.Fatalf("status %v: %s", args, errText(res))
		}
		return out
	}
	if got := taskIDs(get(map[string]any{"ids": []string{"t3", "t5"}}).Tasks); !slices.Equal(got, []string{"t3", "t5"}) {
		t.Errorf("ids filter = %v, want [t3 t5] (asking by id shows a killed task)", got)
	}
	if got := taskIDs(get(map[string]any{"state": []string{"killed"}}).Tasks); !slices.Equal(got, []string{"t5"}) {
		t.Errorf("state=killed = %v", got)
	}
	if got := taskIDs(get(map[string]any{"state": []string{"landed"}}).Tasks); !slices.Equal(got, []string{"t1"}) {
		t.Errorf("state=landed = %v", got)
	}
	if got := get(map[string]any{"since": "1h"}).Tasks; len(got) == 0 {
		t.Error("since=1h dropped tasks spawned just now")
	}
	if got := get(map[string]any{"since": "2999-01-01T00:00:00Z"}).Tasks; len(got) != 0 {
		t.Errorf("since in the future = %v", taskIDs(got))
	}
	if res := callTool(t, a, app.OrchestratorID, "status", map[string]any{"since": "yesterday"}, nil); !res.IsError {
		t.Error("a bad since should fail")
	}
}

// #340: land with nothing queued says so instead of {"results":null}.
func TestLandToolSaysNothingQueued(t *testing.T) {
	a, _ := stackSetup(t)
	var out LandOut
	res := callTool(t, a, app.OrchestratorID, "land", map[string]any{}, &out)
	if res.IsError {
		t.Fatalf("land: %s", errText(res))
	}
	if out.Results == nil || len(out.Results) != 0 {
		t.Errorf("results = %#v, want []", out.Results)
	}
	if !strings.Contains(out.Message, "nothing queued") || !strings.Contains(out.Message, "done") {
		t.Errorf("message = %q", out.Message)
	}
}

type keysTmux struct {
	namedTmux
	sent *[]string
}

func (k keysTmux) SendKeys(_ string, keys ...string) error {
	*k.sent = append(*k.sent, keys...)
	return nil
}

// #340: send_keys takes a plain string as one key.
func TestSendKeysToolAcceptsString(t *testing.T) {
	a, _ := stackSetup(t)
	tk, err := a.Spawn(app.SpawnReq{ID: "t2", Title: "asker", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	var sent []string
	a.Tmux = keysTmux{namedTmux{a.Tmux.(*fakeTmux), map[string]string{tk.Window: "t2-asker"}}, &sent}
	res := callTool(t, a, app.OrchestratorID, "send_keys", map[string]any{"task": "t2", "keys": "2"}, nil)
	if res.IsError {
		t.Fatalf("send_keys with a string key: %s", errText(res))
	}
	res = callTool(t, a, app.OrchestratorID, "send_keys", map[string]any{"task": "t2", "keys": []string{"Down", "Enter"}}, nil)
	if res.IsError {
		t.Fatalf("send_keys with a key list: %s", errText(res))
	}
	if !slices.Equal(sent, []string{"2", "Down", "Enter"}) {
		t.Errorf("sent = %v", sent)
	}
	if res := callTool(t, a, app.OrchestratorID, "send_keys", map[string]any{"task": "t2", "keys": 2}, nil); !res.IsError {
		t.Error("a number is not a key")
	}
}

// #340: a spawn refused for a claim conflict names the owner's state and
// what to do when its agent is gone, not just "message the owner".
func TestSpawnToolConflictHint(t *testing.T) {
	a := orchSetup(t)
	res := callTool(t, a, app.OrchestratorID, "spawn", map[string]any{"title": "fix api", "prompt": "x", "claims": []string{"api/**"}}, nil)
	msg := errText(res)
	if !res.IsError || !strings.Contains(msg, "claim conflict") {
		t.Fatalf("spawn over t2's claims: %s", msg)
	}
	for _, want := range []string{"t2 (running)", "kill t2", "gone"} {
		if !strings.Contains(msg, want) {
			t.Errorf("conflict message lacks %q: %s", want, msg)
		}
	}
}

// #340: a spawn that only covers landed work says the owner's agent is
// usually gone and offers confirm=true for a repair.
func TestSpawnToolRepairHint(t *testing.T) {
	a, _ := stackSetup(t)
	res := callTool(t, a, app.OrchestratorID, "spawn", map[string]any{"title": "fix readme", "prompt": "x", "claims": []string{"README.md"}}, nil)
	msg := errText(res)
	if !res.IsError {
		t.Fatal("spawn over landed work went ahead without confirm")
	}
	for _, want := range []string{"t1 (landed)", "confirm=true", "gone"} {
		if !strings.Contains(msg, want) {
			t.Errorf("needs-confirm message lacks %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "message the owning task") {
		t.Errorf("still tells the orchestrator to message a gone owner: %s", msg)
	}
}
