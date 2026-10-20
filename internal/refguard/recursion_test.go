package refguard

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/lintgate"
)

// insideHooksRepo is a repo whose core.hooksPath is a tracked directory inside
// the checkout (#691), shipping a pre-commit and no reference-transaction.
func insideHooksRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	mustGit(t, root, "init", "-q", "-b", "main")
	mustGit(t, root, "config", "user.email", "t@example.com")
	mustGit(t, root, "config", "user.name", "t")
	p := filepath.Join(root, "git/hooks/pre-commit")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, root, "add", "-A")
	mustGit(t, root, "commit", "-qm", "init")
	mustGit(t, root, "config", "core.hooksPath", "git/hooks")
	return root
}

// gitTimeout runs git in its own process group under a deadline and kills the
// whole group when it expires, so a hook loop that regresses is stopped
// instead of forking until the machine dies.
func gitTimeout(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(20 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		t.Fatalf("git %v did not finish: hooks are looping", args)
		return "", nil
	}
}

// #691: with core.hooksPath inside the checkout, the repo-hook wrapper and the
// ref guard called each other forever.
func TestInstallInsideHooksPathDoesNotLoop(t *testing.T) {
	root := insideHooksRepo(t)
	for i := 0; i < 2; i++ {
		if err := Install(root, "/nonexistent/saddle"); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := gitTimeout(t, root, "update-ref", "refs/x", "HEAD"); err != nil {
		t.Fatalf("update-ref: %v\n%s", err, out)
	}
	for _, name := range lintgate.GitHooks {
		b, err := os.ReadFile(filepath.Join(root, "git/hooks", name+lintgate.ChainSuffix))
		if err != nil {
			continue
		}
		if strings.Contains(string(b), lintgate.RepoHookMarker) && strings.Contains(string(b), "git/hooks/"+name) {
			t.Fatalf("%s.pre-saddle delegates back to the hooks dir:\n%s", name, b)
		}
	}
	if cs, err := Cycles(root); err != nil || len(cs) != 0 {
		t.Fatalf("cycles after install: %v, %v", cs, err)
	}
}

// Installs from before the fix left a wrapper that runs the ref guard; init
// deletes it, and Cycles reports it until then.
func TestInstallRepairsRecursiveWrapper(t *testing.T) {
	root := insideHooksRepo(t)
	if err := Install(root, "/nonexistent/saddle"); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Join(root, "git/hooks")
	bad := filepath.Join(hooks, "reference-transaction"+lintgate.ChainSuffix)
	s := lintgate.Shipped{Name: "reference-transaction", Rel: "git/hooks/reference-transaction", Kind: lintgate.KindHook, Source: "git/hooks/reference-transaction"}
	if err := os.WriteFile(bad, []byte(repoHookScript(s)), 0o755); err != nil {
		t.Fatal(err)
	}
	cs, err := Cycles(root)
	if err != nil || len(cs) != 1 || cs[0] != "reference-transaction" {
		t.Fatalf("Cycles = %v, %v", cs, err)
	}
	if err := Install(root, "/nonexistent/saddle"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(bad); err == nil {
		t.Fatal("recursive wrapper not deleted")
	}
	if cs, _ := Cycles(root); len(cs) != 0 {
		t.Fatalf("still cyclic: %v", cs)
	}
}

// A saddle-written file, or the hooks dir's own file, is never a shipped hook.
func TestRepoHooksSkipsSaddleWrittenAndSelf(t *testing.T) {
	root := insideHooksRepo(t)
	if err := Install(root, "/nonexistent/saddle"); err != nil {
		t.Fatal(err)
	}
	hs, err := RepoHooks(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		t.Errorf("unexpected shipped hook %+v", h)
	}
}

// The generated scripts stop a loop with an error instead of recursing.
func TestHookScriptsCarryRecursionGuard(t *testing.T) {
	root := insideHooksRepo(t)
	mustGit(t, root, "config", "--unset", "core.hooksPath")
	if err := Install(root, "/nonexistent/saddle"); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Join(root, ".git/hooks")
	scripts := map[string]string{}
	for _, n := range []string{"reference-transaction", "pre-push", "pre-commit", "commit-msg"} {
		b, err := os.ReadFile(filepath.Join(hooks, n))
		if err != nil {
			t.Fatal(err)
		}
		scripts[n] = string(b)
	}
	scripts["wrapper"] = repoHookScript(lintgate.Shipped{Name: "pre-commit", Rel: "git/hooks/pre-commit", Kind: lintgate.KindHook, Source: "git/hooks/pre-commit"})
	for n, body := range scripts {
		name := n
		if n == "wrapper" {
			name = "pre-commit"
		}
		path := filepath.Join(t.TempDir(), "hook")
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", path)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "SADDLE_HOOK_DEPTH="+strings.Repeat(":"+name+":", 4))
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "saddle: hook recursion in "+name+", see saddle doctor") {
			t.Errorf("%s: no recursion error: %v\n%s", n, err, out)
		}
		// Another hook's depth doesn't trip it.
		cmd = exec.Command("sh", path)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "SADDLE_HOOK_DEPTH="+strings.Repeat(":other:", 9), "SADDLE_TRAIN=1")
		if out, _ := cmd.CombinedOutput(); strings.Contains(string(out), "recursion") {
			t.Errorf("%s: tripped by another hook: %s", n, out)
		}
	}
}
