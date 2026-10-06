package refguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/brandonapol/saddle/internal/lintgate"
)

func gitIn(t *testing.T, dir string, env []string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitIn(t, dir, nil, args...)
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return out
}

// hookRepo is a repo that ships git/hooks/pre-commit (logging each run and
// failing on overlap, so a missing lock shows) and a commit-msg hook that
// rejects AI attribution, with one linked worktree, as an agent has.
func hookRepo(t *testing.T) (root, wt, runs string) {
	t.Helper()
	root = t.TempDir()
	runs = filepath.Join(t.TempDir(), "runs")
	busy := filepath.Join(t.TempDir(), "busy")
	mustGit(t, root, "init", "-q", "-b", "main")
	mustGit(t, root, "config", "user.email", "t@example.com")
	mustGit(t, root, "config", "user.name", "t")
	for rel, body := range map[string]string{
		"git/hooks/pre-commit": "#!/bin/sh\necho run >> " + runs + "\nmkdir " + busy + " || { echo 'parallel golangci-lint is running'; exit 1; }\nsleep 0.3\nrmdir " + busy + "\n[ ! -f bad.txt ] || { echo 'bad.txt is not allowed'; exit 1; }\n",
		"git/hooks/commit-msg": "#!/bin/sh\n! grep -qi '^Co-Authored-By: Claude' \"$1\" || { echo 'no AI attribution'; exit 1; }\n",
	} {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustGit(t, root, "add", "-A")
	mustGit(t, root, "commit", "-qm", "ship hooks")
	wt = filepath.Join(t.TempDir(), "wt")
	mustGit(t, root, "worktree", "add", "-q", "-b", "saddle/t1-x", wt)
	return root, wt, runs
}

func commitFile(t *testing.T, dir, name, msg string, env ...string) (string, error) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "add", name)
	return gitIn(t, dir, env, "commit", "-qm", msg)
}

func runCount(runs string) int {
	b, _ := os.ReadFile(runs)
	return strings.Count(string(b), "run")
}

// #223: the repo's hooks were never installed, so no agent commit ran them.
// saddle init installs them for every worktree.
func TestInstallRunsRepoHooksInWorktrees(t *testing.T) {
	root, wt, runs := hookRepo(t)
	if hs, err := RepoHooks(root); err != nil || len(hs) != 2 || hs[0].Active || hs[1].Active {
		t.Fatalf("before install: %+v, %v", hs, err)
	}
	if err := Install(root, "/nonexistent/saddle"); err != nil {
		t.Fatal(err)
	}
	hs, err := RepoHooks(root)
	if err != nil || len(hs) != 2 {
		t.Fatalf("after install: %+v, %v", hs, err)
	}
	for _, h := range hs {
		if !h.Active || !h.Wrapper {
			t.Fatalf("%s not active after install: %+v", h.Name, h)
		}
	}

	if out, err := commitFile(t, wt, "a.txt", "a\n\nCo-Authored-By: Claude <noreply@anthropic.com>"); err == nil || !strings.Contains(out, "no AI attribution") {
		t.Fatalf("commit-msg didn't run in the worktree: %v\n%s", err, out)
	}
	mustGit(t, wt, "reset", "-q")
	if out, err := commitFile(t, wt, "bad.txt", "bad"); err == nil || !strings.Contains(out, "bad.txt is not allowed") {
		t.Fatalf("pre-commit didn't run in the worktree: %v\n%s", err, out)
	}
	mustGit(t, wt, "rm", "-q", "-f", "--cached", "bad.txt")
	_ = os.Remove(filepath.Join(wt, "bad.txt"))
	before := runCount(runs)
	if out, err := commitFile(t, wt, "a.txt", "a"); err != nil {
		t.Fatalf("clean commit: %v\n%s", err, out)
	}
	if runCount(runs) != before+1 {
		t.Fatal("pre-commit didn't run on a clean commit")
	}

	// Fast path: the same tree just passed, so with SADDLE_FAST_HOOK=1 an
	// amend checks nothing; without it, it checks again.
	n := runCount(runs)
	if out, err := gitIn(t, wt, []string{"SADDLE_FAST_HOOK=1"}, "commit", "-q", "--amend", "-m", "a again"); err != nil || !strings.Contains(out, "already passed") {
		t.Fatalf("fast amend: %v\n%s", err, out)
	}
	if runCount(runs) != n {
		t.Fatal("SADDLE_FAST_HOOK=1 ran the hook on a tree that just passed")
	}
	if _, err := gitIn(t, wt, nil, "commit", "-q", "--amend", "-m", "a once more"); err != nil || runCount(runs) != n+1 {
		t.Fatal("without SADDLE_FAST_HOOK the hook should run")
	}
	// The train runs the repo's gate itself.
	if _, err := commitFile(t, wt, "train.txt", "train", "SADDLE_TRAIN=1"); err != nil || runCount(runs) != n+1 {
		t.Fatalf("the train ran the repo's hook: %v", err)
	}
}

// Many agents committing at once run the heavy hook one at a time.
func TestRepoHooksQueueAcrossWorktrees(t *testing.T) {
	root, wt, _ := hookRepo(t)
	if _, err := InstallRepoHooks(root); err != nil {
		t.Fatal(err)
	}
	wt2 := filepath.Join(t.TempDir(), "wt2")
	mustGit(t, root, "worktree", "add", "-q", "-b", "saddle/t2-y", wt2)
	var wg sync.WaitGroup
	outs := make([]string, 3)
	errs := make([]error, 3)
	for i, dir := range []string{wt, wt2, root} {
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(dir), 0o644); err != nil {
			t.Fatal(err)
		}
		mustGit(t, dir, "add", "f.txt")
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = gitIn(t, dir, nil, "commit", "-qm", "f")
		}()
	}
	wg.Wait()
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("concurrent commit %d collided: %v\n%s", i, errs[i], outs[i])
		}
	}
}

// A hook someone else installed is left alone; pre-push goes in the ref
// guard's chained slot.
func TestInstallRepoHooksLeavesForeignHook(t *testing.T) {
	root, _, _ := hookRepo(t)
	if err := os.WriteFile(filepath.Join(root, "git/hooks/pre-push"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Join(root, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := "#!/bin/sh\n# mine\n"
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte(mine), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Install(root, "/nonexistent/saddle"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(hooks, "pre-commit")); string(b) != mine {
		t.Fatalf("foreign pre-commit clobbered:\n%s", b)
	}
	b, err := os.ReadFile(filepath.Join(hooks, "pre-push"+lintgate.ChainSuffix))
	if err != nil || !strings.Contains(string(b), lintgate.RepoHookMarker) {
		t.Fatalf("pre-push wrapper not in the chained slot: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(hooks, "pre-push")); !strings.Contains(string(b), marker) {
		t.Fatal("the ref guard's pre-push was replaced")
	}
	hs, _ := RepoHooks(root)
	for _, h := range hs {
		if !h.Active {
			t.Fatalf("%s inactive: %+v", h.Name, h)
		}
	}
}
