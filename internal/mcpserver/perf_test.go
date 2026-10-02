package mcpserver

import (
	"fmt"
	"os/exec"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// perfSetup opens a repo whose train has landed n tasks, each on its own
// branch at its own commit, with a pending notice apiece.
func perfSetup(tb testing.TB, n int) *app.App {
	tb.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		tb.Skip("git not installed")
	}
	tb.Setenv("XDG_CONFIG_HOME", tb.TempDir())
	tb.Setenv("SADDLE_ROOT", "")
	tb.Setenv("SADDLE_TASK", "")
	tb.Setenv("GIT_AUTHOR_NAME", "t")
	tb.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	tb.Setenv("GIT_COMMITTER_NAME", "t")
	tb.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	root := tb.TempDir()
	run := func(args ...string) string {
		out, err := gitx.Run(root, args...)
		if err != nil {
			tb.Fatal(err)
		}
		return out
	}
	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "init")
	a, err := app.Open(root)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { a.Close() })
	a.Tmux = &fakeTmux{windows: map[string]bool{}}

	prev := run("rev-parse", "HEAD")
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("t%d", i)
		run("commit", "-q", "--allow-empty", "-m", id)
		head := run("rev-parse", "HEAD")
		branch := "saddle/" + id
		run("branch", branch, head)
		must := func(err error) {
			if err != nil {
				tb.Fatal(err)
			}
		}
		must(a.Store.CreateTask(store.Task{ID: id, Title: id, Role: store.RoleWorker, Branch: branch, Status: store.Landed}))
		must(a.Store.Enqueue(id))
		must(a.Store.SetTrain(id, store.TrainOK, prev+".."+head, false))
		must(a.Store.Notify(id, store.NoticeInfo, "hello"))
		prev = head
	}
	return a
}

// gitCalls returns how many git processes fn started.
func gitCalls(tb testing.TB, fn func() error) int64 {
	tb.Helper()
	before := gitx.Calls()
	if err := fn(); err != nil {
		tb.Fatal(err)
	}
	return gitx.Calls() - before
}

// Status runs every second in the TUI and on every orchestrator status call.
// Landed tasks pile up for the life of a repo, so the git work it does must
// not grow with them.
func TestStatusGitCallsDontGrowWithLandedTasks(t *testing.T) {
	status := func(a *app.App) func() error {
		return func() error {
			st, err := Status(a)
			if err == nil && len(st.Warnings) > 0 {
				err = fmt.Errorf("unexpected warnings: %v", st.Warnings)
			}
			return err
		}
	}
	few := gitCalls(t, status(perfSetup(t, 2)))
	many := gitCalls(t, status(perfSetup(t, 30)))
	if many != few {
		t.Fatalf("Status ran %d git processes with 2 landed tasks and %d with 30; want the same", few, many)
	}
}

// Tasks is what the TUI polls. It reads only the store, never git.
func TestTasksRunsNoGit(t *testing.T) {
	a := perfSetup(t, 5)
	var ts []TaskView
	n := gitCalls(t, func() (err error) { ts, err = Tasks(a); return err })
	if n != 0 {
		t.Fatalf("Tasks ran %d git processes, want 0", n)
	}
	if len(ts) != 5 {
		t.Fatalf("got %d tasks, want 5", len(ts))
	}
	for _, tv := range ts {
		if tv.Notices != 1 {
			t.Fatalf("%s has %d pending notices, want 1", tv.ID, tv.Notices)
		}
	}
}

// Drift still reports a landed branch that moved, after batching.
func TestStatusStillWarnsOnDrift(t *testing.T) {
	a := perfSetup(t, 3)
	if _, err := gitx.Run(a.Root, "branch", "-f", "saddle/t2", "saddle/t1"); err != nil {
		t.Fatal(err)
	}
	if _, err := gitx.Run(a.Root, "branch", "-D", "saddle/t3"); err != nil {
		t.Fatal(err)
	}
	st, err := Status(a)
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Warnings) != 2 {
		t.Fatalf("warnings = %q, want t2 drifted and t3 missing", st.Warnings)
	}
}

func BenchmarkStatus(b *testing.B) {
	for _, n := range []int{10, 100} {
		a := perfSetup(b, n)
		b.Run(fmt.Sprintf("landed=%d", n), func(b *testing.B) {
			for b.Loop() {
				if _, err := Status(a); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("tasks/landed=%d", n), func(b *testing.B) {
			for b.Loop() {
				if _, err := Tasks(a); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
