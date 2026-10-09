package cli

import (
	"encoding/json"
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

// queueRepo is a repo with t1 and t2 done and queued in that order.
func queueRepo(t *testing.T) *app.App {
	t.Helper()
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
	return a
}

// #25: saddle queue lists, moves, holds and releases waiting entries.
func TestQueueCommands(t *testing.T) {
	queueRepo(t)
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

// #166: saddle queue --json is the queue the mod's snapshot reads, next
// first, with each entry's position, plus who holds the engine lock so the
// mod polls only while saddle runs.
func TestQueueJSON(t *testing.T) {
	a := queueRepo(t)
	runQueue(t, "hold", "t2", "wait")
	var got queueJSON
	if err := json.Unmarshal([]byte(runQueue(t, "--json")), &got); err != nil {
		t.Fatal(err)
	}
	if got.Engine != "" {
		t.Fatalf("engine = %q with no engine running", got.Engine)
	}
	want := []queueEntryJSON{{Position: 1, Task: "t1", State: "queued"}, {Position: 2, Task: "t2", State: "on_hold", Note: "wait"}}
	if len(got.Entries) != 2 {
		t.Fatalf("entries %+v", got.Entries)
	}
	for i, e := range got.Entries {
		e.Seq, e.Attempts = 0, 0
		if e != want[i] {
			t.Fatalf("entry %d = %+v, want %+v", i, e, want[i])
		}
	}

	release, err := a.AcquireLock(app.LockEngine)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := json.Unmarshal([]byte(runQueue(t, "--json")), &got); err != nil || got.Engine != app.LockEngine {
		t.Fatalf("engine = %q (%v) while the engine runs", got.Engine, err)
	}
}

// An empty queue is [] in JSON, never null, so a reader needn't guard it.
func TestQueueJSONEmpty(t *testing.T) {
	a := automergeRepo(t)
	t.Chdir(a.Root)
	if out := runQueue(t, "--json"); !strings.Contains(out, `"entries": []`) {
		t.Fatalf("empty queue json:\n%s", out)
	}
}

func TestQueueRegistered(t *testing.T) {
	if c, _, err := Root().Find([]string{"queue", "hold"}); err != nil || c.Name() != "hold" {
		t.Fatalf("saddle queue hold not registered: %v", err)
	}
}
