package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
)

// trainSetup is setup with tests explicitly off, so land doesn't refuse.
func trainSetup(t *testing.T) *App {
	t.Helper()
	a, _ := setup(t)
	a.Cfg.Test.Cmd = "none"
	return a
}

// landTask spawns a task, commits files on its branch and lands it.
func landTask(t *testing.T, a *App, title string, commits ...map[string]string) store.Task {
	t.Helper()
	tk, err := a.Spawn(SpawnReq{Title: title})
	must(t, err)
	for i, files := range commits {
		for rel, body := range files {
			write(t, tk.Worktree, rel, body)
		}
		msg := title
		if i > 0 {
			msg += " part " + string(rune('1'+i))
		}
		commitAll(t, tk.Worktree, msg)
	}
	must(t, a.Done(tk.ID, title+" summary"))
	rs, err := a.Land()
	must(t, err)
	for _, r := range rs {
		if r.State != store.TrainOK {
			t.Fatalf("land %s: %s %s", r.Task, r.State, r.Note)
		}
	}
	got, err := a.Store.Task(tk.ID)
	must(t, err)
	return got
}

func TestLandRefusesWithoutTestCmd(t *testing.T) {
	a := trainSetup(t)
	tk, err := a.Spawn(SpawnReq{Title: "one"})
	must(t, err)
	write(t, tk.Worktree, "one.txt", "one\n")
	commitAll(t, tk.Worktree, "one")
	must(t, a.Done(tk.ID, "one"))
	before := git(t, a.Root, "rev-parse", a.Cfg.Integration)

	a.Cfg.Test.Cmd = ""
	_, err = a.Land()
	if err == nil || !strings.Contains(err.Error(), `set [test] cmd in .saddle/config.toml, or cmd = "none" to land untested`) {
		t.Fatalf("land without a test cmd: err = %v", err)
	}
	es, err := a.Store.Train()
	must(t, err)
	if len(es) != 1 || es[0].State != store.Queued || es[0].Attempts != 0 {
		t.Fatalf("queue changed: %+v", es)
	}
	if got := git(t, a.Root, "rev-parse", a.Cfg.Integration); got != before {
		t.Fatalf("integration moved to %s", got)
	}

	a.Cfg.Test.Cmd = "none"
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TrainOK {
		t.Fatalf("land with cmd = none: %+v", rs)
	}
}

func TestDetectTestCmd(t *testing.T) {
	a, _ := setup(t)
	write(t, a.Root, "go.mod", "module x\n")
	write(t, a.Root, "Makefile", "GO := go\n\ncheck: vet test\n\tgo vet ./...\n")
	a.Cfg.Test.Cmd = ""
	must(t, a.Init())
	cfg, err := config.Load(a.Root)
	must(t, err)
	if cfg.Test.Cmd != "make check" {
		t.Fatalf("detected test cmd = %q, want make check", cfg.Test.Cmd)
	}

	cases := map[string]string{
		"go.mod":       "go test ./...",
		"package.json": "npm test",
		"Cargo.toml":   "cargo test",
	}
	for file, want := range cases {
		dir := t.TempDir()
		write(t, dir, file, "\n")
		write(t, dir, "Makefile", "check := yes\nbuild:\n")
		if got := DetectTestCmd(dir); got != want {
			t.Errorf("DetectTestCmd with %s = %q, want %q", file, got, want)
		}
	}
	if got := DetectTestCmd(t.TempDir()); got != "" {
		t.Errorf("DetectTestCmd on an empty repo = %q", got)
	}
}
