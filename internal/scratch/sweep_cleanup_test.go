package scratch

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// #801: Go's module cache makes read-only dirs; the sweep still removes a
// stale tree holding them, without an error.
func TestSweepRemovesReadOnlyTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	task := filepath.Join(root, RepoKey("/repo"), "task-t1")
	ro := filepath.Join(task, "gomod", "pkg@v1")
	mk(t, ro, 10, 0)
	must(t, os.Chmod(ro, 0o555))
	must(t, os.Chmod(filepath.Dir(ro), 0o555))
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(ro), 0o755); _ = os.Chmod(ro, 0o755) })
	old := time.Now().Add(-3 * time.Hour)
	must(t, os.Chtimes(task, old, old))
	must(t, os.Chtimes(filepath.Join(task, "gomod"), old, old))

	res := Sweep(Options{Root: root, MaxAge: time.Hour, Holders: noHolders, RepoKey: RepoKey("/repo")})
	if len(res.Errs) > 0 {
		t.Fatalf("sweep errors: %v", res.Errs)
	}
	if exists(task) {
		t.Fatalf("read-only tree %s survived the sweep: %+v", task, res.Kept)
	}
}

// #801: an empty repo-key dir is removed once it is stale.
func TestSweepRemovesEmptyRepoKeyDir(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	key := filepath.Join(root, RepoKey("/gone"))
	must(t, os.MkdirAll(key, 0o755))
	old := time.Now().Add(-3 * time.Hour)
	must(t, os.Chtimes(key, old, old))
	fresh := filepath.Join(root, RepoKey("/new"))
	must(t, os.MkdirAll(fresh, 0o755))

	res := Sweep(Options{Root: root, MaxAge: time.Hour, Holders: noHolders})
	if exists(key) {
		t.Fatalf("empty repo-key dir %s survived: %+v", key, res)
	}
	if !exists(fresh) {
		t.Fatal("a fresh repo-key dir was removed: a spawn may be creating its task dir")
	}
}

// #801: another repo's task dirs aren't this sweep's to judge: their
// holders are that repo's live tasks, which this repo can't see.
func TestSweepLeavesOtherReposTaskDirs(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	other := filepath.Join(root, RepoKey("/b"))
	idle := filepath.Join(other, "task-t5")
	mk(t, idle, 10, 3*time.Hour)
	gate := filepath.Join(other, "gate-123")
	mk(t, gate, 10, 3*time.Hour)

	Sweep(Options{Root: root, MaxAge: time.Nanosecond, Holders: noHolders, RepoKey: RepoKey("/a")})
	if !exists(idle) {
		t.Fatal("a sweep from repo A removed repo B's task dir")
	}
	if exists(gate) {
		t.Fatal("a stale gate dir of another repo survived")
	}
}
