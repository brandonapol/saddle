package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/automerge"
	"github.com/spf13/cobra"
)

func runCmd(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	return out.String(), err
}

// automergeRepo is a repo with one spawned task, t1, and no GitHub: a gh
// that fails, so nothing can reach the real one.
func automergeRepo(t *testing.T) *app.App {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SADDLE_ROOT", "")
	t.Setenv("SADDLE_TRAIN", "")
	t.Setenv("SADDLE_TASK", "")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\necho 'no GitHub in tests' >&2\nexit 1\n"), 0o755); err != nil {
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
	a, err := app.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	a.Tmux = &fakeTmux{}
	if _, err := a.Spawn(app.SpawnReq{ID: "t1", Title: "t1"}); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	return a
}

// #152: saddle automerge on|off|status|hold|release.
func TestAutomergeCommands(t *testing.T) {
	automergeRepo(t)
	must := func(args ...string) string {
		t.Helper()
		out, err := runCmd(t, automergeCmd(), args...)
		if err != nil {
			t.Fatalf("saddle automerge %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	if out := must("status"); !strings.Contains(out, "auto-merge: off (config)") {
		t.Fatalf("default status:\n%s", out)
	}
	must("on")
	if out := must(); !strings.Contains(out, "auto-merge: on (runtime)") {
		t.Fatalf("after on:\n%s", out)
	}
	if out := must("hold", "t1"); !strings.Contains(out, "t1's stack is held") {
		t.Fatalf("hold:\n%s", out)
	}
	if out := must("status"); !strings.Contains(out, "held: t1") {
		t.Fatalf("status after hold:\n%s", out)
	}
	if out := must("status", "--json"); !strings.Contains(out, `"holds": [`) {
		t.Fatalf("json status:\n%s", out)
	}
	must("release", "t1")
	if out := must("status"); strings.Contains(out, "held:") {
		t.Fatalf("status after release:\n%s", out)
	}
	if _, err := runCmd(t, automergeCmd(), "release", "t1"); err == nil {
		t.Fatal("release of an unheld stack: want error")
	}
	if _, err := runCmd(t, automergeCmd(), "hold", "t404"); err == nil {
		t.Fatal("hold of an unknown task: want error")
	}
	must("off")
	if out := must("status"); !strings.Contains(out, "auto-merge: off (runtime)") {
		t.Fatalf("after off:\n%s", out)
	}
}

func TestStackCommands(t *testing.T) {
	automergeRepo(t)
	out, err := runCmd(t, stackCmd())
	if err != nil || !strings.Contains(out, "no open PR stacks") {
		t.Fatalf("saddle stack: %v\n%s", err, out)
	}
	if out, err := runCmd(t, stackCmd(), "rebase", "t1"); err == nil || !strings.Contains(err.Error(), "isn't in the PR stack") {
		t.Fatalf("rebase of an unlanded task: %v\n%s", err, out)
	}
}

func TestRootRegistersAutomerge(t *testing.T) {
	for _, name := range []string{"automerge", "stack"} {
		if c, _, err := Root().Find([]string{name}); err != nil || c.Name() != name {
			t.Errorf("saddle %s not registered: %v", name, err)
		}
	}
}

// Status says why each stack isn't merged, the busy train lock, and when
// the watcher checks next.
func TestWriteAutomergeSaysWhyAndWhen(t *testing.T) {
	at := time.Date(2026, 10, 3, 12, 41, 0, 0, time.Local)
	st := automerge.Status{Enabled: true, Source: automerge.SourceRuntime, Busy: true, BusySince: at, Next: at.Add(5 * time.Second),
		Stacks: []automerge.Stack{
			{ID: "t61", Next: "pr/191", Why: "its CI is red", Nodes: []automerge.Node{{Task: "t61", PR: "pr/191", Base: "main"}}},
			{ID: "t58", Next: "pr/185", Ready: true, Blocked: "the train lock is busy", Nodes: []automerge.Node{{Task: "t58", PR: "pr/185", Base: "main"}}},
		}}
	var out strings.Builder
	writeAutomerge(&out, st, "main")
	for _, want := range []string{
		"train lock busy since 12:41:00",
		"next check: 12:41:05",
		"next: pr/191 waits: its CI is red",
		"next: pr/185 is ready to merge; the train lock is busy",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}
}
