package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// staleFixture makes worktree-looking directories under .saddle/worktrees.
func staleFixture(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, n := range names {
		dir := filepath.Join(root, ".saddle", "worktrees", n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// #803 step 5: status counts the worktrees of ended or gone tasks and says
// to run gc; --json carries the count.
func TestStatusShowsStaleWorktrees(t *testing.T) {
	a := storeApp(t)
	addTask(t, a, store.Task{ID: "t1", Title: "live", Status: store.Running})
	addTask(t, a, store.Task{ID: "t2", Title: "killed", Status: store.Killed})
	addTask(t, a, store.Task{ID: "t3", Title: "landed", Status: store.Landed})
	staleFixture(t, a.Root, "t1-live", "t2-killed", "t3-landed", "t9-gone")

	out := runStatus(t, a)
	if !strings.Contains(out, "3 stale worktrees") || !strings.Contains(out, "saddle gc") {
		t.Fatalf("status lacks the stale worktree line:\n%s", out)
	}
	var js struct {
		StaleWorktrees int `json:"stale_worktrees"`
	}
	if err := json.Unmarshal([]byte(runStatus(t, a, "--json")), &js); err != nil {
		t.Fatal(err)
	}
	if js.StaleWorktrees != 3 {
		t.Fatalf("json stale_worktrees = %d, want 3", js.StaleWorktrees)
	}
}

// With only live worktrees, status says nothing about stale ones and the
// JSON field is absent.
func TestStatusQuietWithoutStaleWorktrees(t *testing.T) {
	a := storeApp(t)
	addTask(t, a, store.Task{ID: "t1", Title: "live", Status: store.Running})
	staleFixture(t, a.Root, "t1-live")
	if out := runStatus(t, a); strings.Contains(out, "stale") {
		t.Fatalf("status mentions stale worktrees with none:\n%s", out)
	}
	if js := runStatus(t, a, "--json"); strings.Contains(js, "stale_worktrees") {
		t.Fatalf("json carries stale_worktrees at zero:\n%s", js)
	}
}
