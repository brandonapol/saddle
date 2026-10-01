package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// trainSetup is setup with tests explicitly off, so land doesn't refuse.
func trainSetup(t *testing.T) *App {
	t.Helper()
	a, _ := setup(t)
	a.Cfg.Test.Cmd = "none"
	return a
}

// originWithGh gives the repo a bare origin holding base, and puts a fake gh on
// PATH. It returns the origin's path and a func reading gh's call log.
func originWithGh(t *testing.T, a *App) (string, func() []string) {
	t.Helper()
	origin := t.TempDir()
	git(t, origin, "init", "-q", "--bare", "-b", "main")
	git(t, a.Root, "remote", "add", "origin", origin)
	git(t, a.Root, "push", "-q", "origin", a.Cfg.Base)
	git(t, a.Root, "fetch", "-q", "origin")

	bin := t.TempDir()
	log := filepath.Join(bin, "gh.log")
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
if [ "$1 $2" = "pr create" ]; then
	n=$(grep -c '^pr create' "` + log + `")
	echo "https://github.com/o/r/pull/$n"
fi
`
	must(t, os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return origin, func() []string {
		b, err := os.ReadFile(log)
		if os.IsNotExist(err) {
			return nil
		}
		must(t, err)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

// remoteRev is the commit a branch points at in the bare origin, or "".
func remoteRev(t *testing.T, origin, branch string) string {
	t.Helper()
	out, _ := gitx.Run(origin, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return out
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

func TestPRsPushesLandedSHAs(t *testing.T) {
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	t1 := landTask(t, a, "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "two", map[string]string{"two.txt": "two\n"})
	landed := map[string]string{t1.ID: git(t, a.Root, "rev-parse", a.Cfg.Integration+"~1"), t2.ID: git(t, a.Root, "rev-parse", a.Cfg.Integration)}

	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	for _, tk := range []store.Task{t1, t2} {
		if got := remoteRev(t, origin, tk.Branch); got != landed[tk.ID] {
			t.Fatalf("%s: remote %s = %q, landed %s", tk.ID, tk.Branch, got, landed[tk.ID])
		}
	}
	if len(ghLog()) == 0 {
		t.Fatal("gh was not called")
	}
}

func TestPRsRefusesDriftedBranch(t *testing.T) {
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	t1 := landTask(t, a, "one", map[string]string{"one.txt": "one\n"})
	landed := git(t, a.Root, "rev-parse", t1.Branch)

	// Something rewrites the landed branch after the train recorded it.
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, a.Root, "worktree", "add", "-q", wt, t1.Branch)
	git(t, wt, "commit", "-q", "--amend", "-m", "one, amended")
	drifted := git(t, a.Root, "rev-parse", t1.Branch)

	_, err := a.PRs()
	want := t1.ID + ": landed " + landed[:12] + ", branch " + drifted[:12]
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("PRs on a drifted branch: err = %v, want %q", err, want)
	}
	if got := remoteRev(t, origin, t1.Branch); got != "" {
		t.Fatalf("drifted branch was pushed: %s", got)
	}
	if l := ghLog(); len(l) > 0 {
		t.Fatalf("gh called: %v", l)
	}
}
