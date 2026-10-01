package gitx_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/gitx/symbols"
)

// isolate keeps the user's git config and hooks out of the test repos.
func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, k := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

const libGo = `package lib

func A() int {
	return 1
}

func B() int {
	return 2
}
`

// setup makes a repo whose "integration" branch has lib.go, and a task
// worktree on branch "task" cut from it.
func setup(t *testing.T) (root, wt string) {
	t.Helper()
	isolate(t)
	base := t.TempDir()
	root = filepath.Join(base, "repo")
	wt = filepath.Join(base, "wt")
	run(t, base, "init", "-q", "-b", "integration", root)
	write(t, root, "lib.go", libGo)
	write(t, root, ".gitignore", "build/\n")
	run(t, root, "add", ".")
	run(t, root, "commit", "-q", "-m", "init")
	run(t, root, "worktree", "add", "-q", "-b", "task", wt, "integration")
	return root, wt
}

func newWatcher(t *testing.T) *gitx.Watcher {
	t.Helper()
	w, err := gitx.NewWatcher(gitx.WatcherConfig{
		Integration: "integration",
		Symbols:     symbols.Default(),
		Debounce:    20 * time.Millisecond,
		MaxWait:     200 * time.Millisecond,
		OnError:     func(task string, err error) { t.Logf("watcher error (%s): %v", task, err) },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	return w
}

// await reads deltas until match returns true, failing after a generous
// timeout. Unmatched deltas are skipped, so the test does not depend on how
// events happened to be batched.
func await[D gitx.Delta](t *testing.T, w *gitx.Watcher, what string, match func(D) bool) D {
	t.Helper()
	timeout := time.After(10 * time.Second)
	var seen []string
	for {
		select {
		case d, ok := <-w.Events():
			if !ok {
				t.Fatalf("events closed waiting for %s", what)
			}
			seen = append(seen, fmt.Sprintf("%T%+v", d, strip(d)))
			if x, ok := d.(D); ok && match(x) {
				return x
			}
		case <-timeout:
			t.Fatalf("timed out waiting for %s; saw:\n%s", what, strings.Join(seen, "\n"))
		}
	}
}

// strip drops the embedded state so failure output stays readable.
func strip(d gitx.Delta) any {
	switch d := d.(type) {
	case gitx.FileDirty:
		return []any{d.Path, d.Status.Code(), d.Clean}
	case gitx.SymbolTouched:
		return []any{d.Path, d.Name, d.Cleared}
	case gitx.RenameDetected:
		return d.PathRename
	case gitx.Committed:
		return []any{d.Old, d.New, len(d.Commits)}
	case gitx.Rebased:
		return []any{d.Old, d.New}
	}
	return d
}

func TestWatcherLifecycle(t *testing.T) {
	root, wt := setup(t)
	w := newWatcher(t)
	if err := w.Add("t1", wt, ""); err != nil {
		t.Fatal(err)
	}
	st, ok := w.State("t1")
	if !ok || st.Branch != "task" || st.Head == "" || len(st.Files) != 0 || st.Ahead != 0 {
		t.Fatalf("initial state = %+v", st)
	}

	// Edit inside B, in a new subdirectory too: FileDirty + SymbolTouched(B).
	write(t, wt, "lib.go", strings.Replace(libGo, "return 2", "return 22", 1))
	await(t, w, "FileDirty lib.go", func(d gitx.FileDirty) bool { return d.Path == "lib.go" && !d.Clean })
	sym := await(t, w, "SymbolTouched B", func(d gitx.SymbolTouched) bool { return d.Name == "B" })
	if sym.Path != "lib.go" || sym.Kind != "func" || sym.Cleared {
		t.Fatalf("symbol delta = %+v", sym.TouchedSymbol)
	}
	write(t, wt, "pkg/sub/new.go", "package sub\n\ntype N struct{}\n")
	await(t, w, "untracked file in new dir", func(d gitx.FileDirty) bool { return d.Path == "pkg/sub/new.go" && d.Status.Untracked })
	await(t, w, "symbol in untracked file", func(d gitx.SymbolTouched) bool { return d.Path == "pkg/sub/new.go" && d.Name == "N" })

	// Commit: Committed, files clean, ahead 1.
	run(t, wt, "add", "-A")
	run(t, wt, "commit", "-q", "-m", "change B")
	c := await(t, w, "Committed", func(d gitx.Committed) bool { return true })
	if len(c.Commits) != 1 || c.Commits[0] != c.New || c.Branch != "task" {
		t.Fatalf("committed = %+v", strip(c))
	}
	await(t, w, "lib.go clean", func(d gitx.FileDirty) bool { return d.Path == "lib.go" && d.Clean })
	st, _ = w.State("t1")
	if st.Ahead != 1 || st.Behind != 0 || st.CommitsSinceBase != 1 {
		t.Fatalf("after commit: ahead=%d behind=%d since=%d", st.Ahead, st.Behind, st.CommitsSinceBase)
	}
	// B stays touched: it differs from the base even though it is committed.
	if !hasSymbol(st, "B") || hasSymbol(st, "A") {
		t.Fatalf("touched after commit = %+v", st.Symbols)
	}

	// Staged rename.
	run(t, wt, "mv", "lib.go", "core.go")
	r := await(t, w, "RenameDetected", func(d gitx.RenameDetected) bool { return !d.Committed })
	if r.Old != "lib.go" || r.New != "core.go" {
		t.Fatalf("rename = %+v", r.PathRename)
	}
	run(t, wt, "commit", "-q", "-m", "rename")
	await(t, w, "committed rename", func(d gitx.RenameDetected) bool { return d.Committed && d.New == "core.go" })

	// Integration moves: behind grows; then the task rebases onto it.
	write(t, root, "other.go", "package lib\n\nfunc O() {}\n")
	run(t, root, "add", ".")
	run(t, root, "commit", "-q", "-m", "other")
	waitState(t, w, "t1", "behind 1", func(s gitx.State) bool { return s.Behind == 1 })

	run(t, wt, "rebase", "-q", "integration")
	rb := await(t, w, "Rebased", func(d gitx.Rebased) bool { return true })
	st = rb.State
	if st.Behind != 0 || st.Ahead != 2 || rb.Base != st.Integration || st.Rebasing {
		t.Fatalf("after rebase: %+v", st)
	}
}

func hasSymbol(st gitx.State, name string) bool {
	for _, s := range st.Symbols {
		if s.Name == name {
			return true
		}
	}
	return false
}

func waitState(t *testing.T, w *gitx.Watcher, task, what string, ok func(gitx.State) bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s, ready := w.State(task); ready && ok(s) {
			return
		}
		select {
		case <-w.Events():
		case <-time.After(10 * time.Millisecond):
		}
	}
	s, _ := w.State(task)
	t.Fatalf("timed out waiting for %s; state = %+v", what, s)
}

// TestDebounceCoalesces checks that a burst of writes produces one re-read,
// not one per write.
func TestDebounceCoalesces(t *testing.T) {
	_, wt := setup(t)
	var reads atomic.Int32
	counter := countingExtractor{reads: &reads}
	w, err := gitx.NewWatcher(gitx.WatcherConfig{
		Integration: "integration",
		Symbols:     counter,
		Debounce:    300 * time.Millisecond,
		MaxWait:     time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if err := w.Add("t1", wt, ""); err != nil {
		t.Fatal(err)
	}
	before := reads.Load()
	for i := range 50 {
		write(t, wt, "burst.txt", fmt.Sprint(i))
	}
	await(t, w, "burst.txt dirty", func(d gitx.FileDirty) bool { return d.Path == "burst.txt" })
	// Supports is called once per snapshot for burst.txt (an untracked file).
	if n := reads.Load() - before; n != 1 {
		t.Fatalf("burst caused %d re-reads, want 1", n)
	}
}

type countingExtractor struct{ reads *atomic.Int32 }

func (c countingExtractor) Supports(path string) bool {
	if path == "burst.txt" {
		c.reads.Add(1)
	}
	return false
}

func (countingExtractor) Symbols(string, []byte) ([]gitx.Symbol, error) { return nil, nil }

func TestIgnoredDirsAreNotWatched(t *testing.T) {
	_, wt := setup(t)
	write(t, wt, "build/out.txt", "x")
	w := newWatcher(t)
	if err := w.Add("t1", wt, ""); err != nil {
		t.Fatal(err)
	}
	write(t, wt, "build/out.txt", "y")
	write(t, wt, "real.txt", "z")
	d := await(t, w, "real.txt", func(d gitx.FileDirty) bool { return true })
	if d.Path != "real.txt" {
		t.Fatalf("first delta for %s, want real.txt", d.Path)
	}
}

func TestRemoveAndClose(t *testing.T) {
	_, wt := setup(t)
	w := newWatcher(t)
	if err := w.Add("t1", wt, ""); err != nil {
		t.Fatal(err)
	}
	if err := w.Add("t1", wt, ""); err == nil {
		t.Fatal("duplicate Add succeeded")
	}
	w.Remove("t1")
	if _, ok := w.State("t1"); ok {
		t.Fatal("state survives Remove")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := <-w.Events(); ok {
		t.Fatal("events not closed")
	}
	if err := w.Add("t2", wt, ""); err == nil {
		t.Fatal("Add after Close succeeded")
	}
}
