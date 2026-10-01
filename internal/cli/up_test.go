package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

type fakeTmux struct{ n int }

func (f *fakeTmux) HasSession() bool                          { return true }
func (f *fakeTmux) NewSession(n, d, c string) (string, error) { return f.NewWindow(n, d, c) }
func (f *fakeTmux) NewWindow(string, string, string) (string, error) {
	f.n++
	return fmt.Sprintf("@%d", f.n), nil
}
func (f *fakeTmux) KillWindow(string) error             { return nil }
func (f *fakeTmux) Alive(string) bool                   { return false }
func (f *fakeTmux) SendText(string, string) error       { return nil }
func (f *fakeTmux) SendKeys(string, ...string) error    { return nil }
func (f *fakeTmux) Capture(string, int) (string, error) { return "", nil }
func (f *fakeTmux) KillSession() error                  { return nil }

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.Run(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// landedPR sets up a repo with an origin and one landed task whose PR a fake
// gh on PATH reports as conflicting with its base. It returns the app and the
// gh call log.
func landedPR(t *testing.T) (*app.App, string) {
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

	bin := t.TempDir()
	log := filepath.Join(bin, "gh.log")
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
case "$1 $2" in
"pr create") echo "https://github.com/o/r/pull/1" ;;
"pr view") echo '{"state":"OPEN","mergeable":"CONFLICTING"}' ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	root := t.TempDir()
	gitRun(t, root, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-qm", "init")
	origin := t.TempDir()
	gitRun(t, origin, "init", "-q", "--bare", "-b", "main")
	gitRun(t, root, "remote", "add", "origin", origin)
	gitRun(t, root, "push", "-q", "origin", "main")
	gitRun(t, root, "fetch", "-q", "origin")

	a, err := app.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	a.Cfg.Test.Cmd = app.NoTestCmd
	a.Cfg.CloseOnLand = true
	a.Tmux = &fakeTmux{}

	tk, err := a.Spawn(app.SpawnReq{ID: "t1", Title: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tk.Worktree, "one.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SADDLE_TASK", tk.ID)
	gitRun(t, tk.Worktree, "add", "-A")
	gitRun(t, tk.Worktree, "commit", "-qm", "one")
	t.Setenv("SADDLE_TASK", "")
	if err := a.Done(tk.ID, "one"); err != nil {
		t.Fatal(err)
	}
	rs, err := a.Land()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 1 || rs[0].State != store.TrainOK {
		t.Fatalf("land = %+v", rs)
	}
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	return a, log
}

// saddle up starts the stack sentinel: one cycle runs against gh right away,
// and the loop stops when its context ends.
func TestStartWatchersRunsSentinel(t *testing.T) {
	a, log := landedPR(t)

	stop := startWatchers(context.Background(), a)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, flagged, err := a.Flag(); err != nil {
			t.Fatal(err)
		} else if flagged {
			break
		}
		if time.Now().After(deadline) {
			b, _ := os.ReadFile(log)
			t.Fatalf("sentinel never flagged the stack; gh calls:\n%s", b)
		}
		time.Sleep(20 * time.Millisecond)
	}

	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("watchers did not stop after their context was cancelled")
	}

	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "pr view https://github.com/o/r/pull/1") {
		t.Fatalf("gh calls = %s, want a pr view of the landed PR", b)
	}
	f, _, _ := a.Flag()
	if f.Task != "t1" || len(f.PRs) != 1 {
		t.Fatalf("flag = %+v, want t1 with its PR labeled", f)
	}
}

func TestStatusShowsFlaggedStack(t *testing.T) {
	a, _ := landedPR(t)
	if err := a.SetFlag(app.StackFlag{Task: "t1", Cause: "GitHub reports its PR conflicts with its base",
		PRs: []string{"https://github.com/o/r/pull/1"}}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(a.Root)
	var out strings.Builder
	cmd := Root()
	cmd.SetArgs([]string{"status"})
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"stack at risk from t1 up: GitHub reports its PR conflicts", "https://github.com/o/r/pull/1", "run restack"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("status output lacks %q:\n%s", want, out.String())
		}
	}
}
