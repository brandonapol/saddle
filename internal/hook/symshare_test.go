package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
)

const shareSrc = "package a\n\nfunc Foo() int {\n\treturn 1\n}\n\nfunc M1() {}\n\nfunc M2() {}\n\nfunc M3() {}\n\n" +
	"func Bar() int {\n\treturn 3\n}\n"

// shareTasks makes owner A (claims a.go) and B, cut from one base in
// symbol mode.
func shareTasks(t *testing.T) (a *app.App, owner, b string) {
	t.Helper()
	t.Cleanup(app.SetShareBudget(time.Minute))
	t.Cleanup(app.SetStaleBudget(time.Minute))
	a = setup(t)
	a.Cfg.Claims.Granularity = config.GranularitySymbol
	if err := os.WriteFile(filepath.Join(a.Root, "a.go"), []byte(shareSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOK(t, a.Root, "add", "a.go")
	gitOK(t, a.Root, "commit", "-qm", "add a.go")
	ot, err := a.Spawn(app.SpawnReq{Title: "owner", Claims: []string{"a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	bt, err := a.Spawn(app.SpawnReq{Title: "b"})
	if err != nil {
		t.Fatal(err)
	}
	return a, ot.ID, bt.ID
}

func editOut(t *testing.T, a *app.App, task, old, new string) (decision, reason string) {
	t.Helper()
	tk, err := a.Store.Task(task)
	if err != nil {
		t.Fatal(err)
	}
	out := run(t, a, task, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": filepath.Join(tk.Worktree, "a.go"), "old_string": old, "new_string": new}})
	hs, _ := out["hookSpecificOutput"].(map[string]any)
	d, _ := hs["permissionDecision"].(string)
	r, _ := hs["permissionDecisionReason"].(string)
	return d, r
}

func editFile(t *testing.T, a *app.App, task, old, new string) {
	t.Helper()
	tk, err := a.Store.Task(task)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(tk.Worktree, "a.go")
	src, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(src), old, new, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// #591: B's Edit of Bar in A's file passes the write guard while A changes
// Foo; B's Edit of Foo is denied naming Foo and quoting A's hunk.
func TestPreToolUseSharesFileByDisjointSymbols(t *testing.T) {
	a, owner, b := shareTasks(t)
	editFile(t, a, owner, "return 1", "return 10")
	if d, r := editOut(t, a, b, "return 3", "return 30"); d == "deny" {
		t.Fatalf("B's Bar edit denied: %s", r)
	}
	editFile(t, a, b, "return 3", "return 30")
	d, r := editOut(t, a, b, "return 1", "return 100")
	if d != "deny" {
		t.Fatalf("B's Foo edit = %q, want deny", d)
	}
	for _, want := range []string{RuleWriteGuard, "`Foo`", owner + "'s current change to a.go", "+\treturn 10"} {
		if !strings.Contains(r, want) {
			t.Errorf("reason %q lacks %q", r, want)
		}
	}
}

// #591, Q1: when the path owner edits a symbol B took first, the deny quotes
// B's hunk, not nothing (the owner has no path owner of its own).
func TestPreToolUseOwnerDeniedQuotesFirstWriter(t *testing.T) {
	a, owner, b := shareTasks(t)
	if d, r := editOut(t, a, b, "return 3", "return 30"); d == "deny" {
		t.Fatalf("B's Bar edit denied: %s", r)
	}
	editFile(t, a, b, "return 3", "return 30")
	d, r := editOut(t, a, owner, "return 3", "return 300")
	if d != "deny" {
		t.Fatalf("owner's Bar edit = %q, want deny", d)
	}
	for _, want := range []string{"`Bar`", b + "'s current change to a.go", "+\treturn 30"} {
		if !strings.Contains(r, want) {
			t.Errorf("reason %q lacks %q", r, want)
		}
	}
}
