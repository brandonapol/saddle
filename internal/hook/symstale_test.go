package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/gitx"
)

func gitOK(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.Run(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// #590: a PreToolUse Edit of a function integration changed after the
// task's base is denied by the write guard, naming the function and
// saddle sync.
func TestPreToolUseDeniesStaleOverwrite(t *testing.T) {
	defer app.SetStaleBudget(time.Minute)()
	a := setup(t)
	a.Cfg.Claims.Granularity = config.GranularitySymbol
	src := "package a\n\nfunc Foo() int {\n\treturn 1\n}\n"
	if err := os.WriteFile(filepath.Join(a.Root, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOK(t, a.Root, "commit", "-qam", "add Foo")
	tk, err := a.Spawn(app.SpawnReq{Title: "b"})
	if err != nil {
		t.Fatal(err)
	}

	scratch := filepath.Join(t.TempDir(), "scratch")
	gitOK(t, a.Root, "worktree", "add", "-q", "--detach", scratch, a.Cfg.Integration)
	if err := os.WriteFile(filepath.Join(scratch, "a.go"), []byte(strings.Replace(src, "return 1", "return 10", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOK(t, scratch, "commit", "-qam", "Foo returns ten")
	head := gitOK(t, scratch, "rev-parse", "HEAD")
	t.Setenv("SADDLE_TRAIN", "1")
	gitOK(t, a.Root, "update-ref", "refs/heads/"+a.Cfg.Integration, head)
	t.Setenv("SADDLE_TRAIN", "")

	out := run(t, a, tk.ID, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": filepath.Join(tk.Worktree, "a.go"), "old_string": "return 1", "new_string": "return 2"}})
	hs, _ := out["hookSpecificOutput"].(map[string]any)
	reason, _ := hs["permissionDecisionReason"].(string)
	if hs["permissionDecision"] != "deny" {
		t.Fatalf("out = %v, want a deny", out)
	}
	for _, want := range []string{RuleWriteGuard, "`Foo`", "Foo returns ten", "saddle sync"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q lacks %q", reason, want)
		}
	}
}
