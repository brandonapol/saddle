package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
)

func runQueue(t *testing.T, args ...string) string {
	t.Helper()
	var out strings.Builder
	cmd := queueCmd()
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("saddle queue %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String()
}

// #25: saddle queue lists, moves, holds and releases waiting entries.
func TestQueueCommands(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SADDLE_ROOT", "")
	t.Setenv("SADDLE_TRAIN", "")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	root := t.TempDir()
	gitRun(t, root, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-qm", "init")
	a, err := app.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	a.Tmux = &fakeTmux{}
	for _, id := range []string{"t1", "t2"} {
		tk, err := a.Spawn(app.SpawnReq{ID: id, Title: id})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tk.Worktree, id+".txt"), []byte(id+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("SADDLE_TASK", id)
		gitRun(t, tk.Worktree, "add", "-A")
		gitRun(t, tk.Worktree, "commit", "-qm", id)
		t.Setenv("SADDLE_TASK", "")
		if err := a.Done(id, id); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)

	if out := runQueue(t); out != "1. t1 queued\n2. t2 queued\n" {
		t.Fatalf("queue:\n%s", out)
	}
	runQueue(t, "move", "t2", "1")
	runQueue(t, "hold", "t1", "needs", "review")
	if out := runQueue(t); out != "1. t2 queued\n2. t1 on_hold: needs review\n" {
		t.Fatalf("queue after move and hold:\n%s", out)
	}
	runQueue(t, "release", "t1")
	if out := runQueue(t); !strings.Contains(out, "2. t1 queued") {
		t.Fatalf("queue after release:\n%s", out)
	}
}
