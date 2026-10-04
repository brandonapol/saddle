package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

// landSecond lands a second task, t2, next to landedPR's t1.
func landSecond(t *testing.T, a *app.App) {
	t.Helper()
	tk, err := a.Spawn(app.SpawnReq{ID: "t2", Title: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tk.Worktree, "two.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, tk.Worktree, "add", "-A")
	gitRun(t, tk.Worktree, "commit", "-qm", "two")
	if err := a.Done(tk.ID, "two"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Land(); err != nil {
		t.Fatal(err)
	}
}

// #211: saddle stack create/add/remove/list/show/delete manage custom stacks.
func TestStackCreateCommands(t *testing.T) {
	a, log := landedPR(t)
	landSecond(t, a)
	t.Chdir(a.Root)

	out := run(t, "stack", "create", "ui", "t2", "t1")
	if !strings.Contains(out, "stack ui: t2 → t1") || !strings.Contains(out, "stack_backend = \"saddle\"") {
		t.Fatalf("create:\n%s", out)
	}
	b, _ := os.ReadFile(log)
	if !strings.Contains(string(b), "pr edit https://github.com/o/r/pull/1 --base saddle/t2-two") {
		t.Fatalf("t1 wasn't put on t2:\n%s", b)
	}
	if out := run(t, "stack", "list"); !strings.Contains(out, "ui") || !strings.Contains(out, "t2 → t1") {
		t.Fatalf("list:\n%s", out)
	}
	if out := run(t, "stack", "show", "ui"); !strings.Contains(out, "1. t2") || !strings.Contains(out, "2. t1") || !strings.Contains(out, "→ saddle/t2-two") {
		t.Fatalf("show:\n%s", out)
	}
	if out := run(t, "stack", "list", "--json"); !strings.Contains(out, `"name": "ui"`) {
		t.Fatalf("list --json:\n%s", out)
	}
	if out := run(t, "stack", "remove", "ui", "t1"); !strings.Contains(out, "stack ui: t2") {
		t.Fatalf("remove:\n%s", out)
	}
	if out := run(t, "stack", "add", "ui", "t1"); !strings.Contains(out, "stack ui: t2 → t1") {
		t.Fatalf("add:\n%s", out)
	}
	if out := run(t, "stack", "delete", "ui"); !strings.Contains(out, "deleted stack ui") {
		t.Fatalf("delete:\n%s", out)
	}
	if out := run(t, "stack", "list"); !strings.Contains(out, "no custom stacks") {
		t.Fatalf("list after delete:\n%s", out)
	}
}

func TestStackCreateRefusesUnlandedTask(t *testing.T) {
	a, _ := landedPR(t)
	t.Chdir(a.Root)
	cmd := Root()
	cmd.SetArgs([]string{"stack", "create", "t1", "nope"})
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), `no task, PR or branch "nope"`) {
		t.Fatalf("err = %v\n%s", err, out.String())
	}
}

// link and merge need stack_backend = "gh-stack" and say so.
func TestStackLinkAndMergeNeedGhStackBackend(t *testing.T) {
	a, _ := landedPR(t)
	landSecond(t, a)
	t.Chdir(a.Root)
	run(t, "stack", "create", "ui", "t1", "t2")
	for _, args := range [][]string{{"stack", "link"}, {"stack", "merge", "ui"}} {
		cmd := Root()
		cmd.SetArgs(args)
		var out strings.Builder
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), `stack_backend = "gh-stack"`) {
			t.Fatalf("%v: err = %v\n%s", args, err, out.String())
		}
	}
}
