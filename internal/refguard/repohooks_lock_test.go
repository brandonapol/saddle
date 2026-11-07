package refguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/lintgate"
)

// #799: the wrapper waits on the hooks lock for at most SADDLE_HOOK_LOCK_WAIT
// seconds. When the lock stays held it skips the heavy hook with a message
// naming the holder (done and the train check the tree anyway) instead of
// blocking the commit with no limit.
func TestRepoHookWrapperBoundsItsLockWait(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock(1) not installed")
	}
	root := insideHooksRepo(t)
	runs := filepath.Join(t.TempDir(), "runs")
	if err := os.WriteFile(filepath.Join(root, "git/hooks/pre-commit"), []byte("#!/bin/sh\necho run >> "+runs+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(root, ".git", "saddle-hooks")
	if err := os.MkdirAll(state, 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(state, "lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "lock.holder"), []byte("done gate for t12\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "wrapper")
	s := lintgate.Shipped{Name: "pre-commit", Rel: "git/hooks/pre-commit", Kind: lintgate.KindHook, Source: "git/hooks/pre-commit"}
	if err := os.WriteFile(script, []byte(repoHookScript(s)), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "SADDLE_HOOK_LOCK_WAIT=1", "SADDLE_TRAIN=", "SADDLE_HOOK_DEPTH=")
	done := make(chan struct{})
	var out []byte
	var runErr error
	go func() { out, runErr = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("wrapper blocked past its -w budget")
	}
	if runErr != nil {
		t.Fatalf("wrapper failed the commit: %v\n%s", runErr, out)
	}
	if _, err := os.Stat(runs); err == nil {
		t.Fatal("the heavy hook ran without the lock")
	}
	if !strings.Contains(string(out), "skipped") || !strings.Contains(string(out), "t12") {
		t.Fatalf("no skip message naming the holder:\n%s", out)
	}
}

// #799: with the lock free the wrapper runs the hook and records itself as
// the holder while it runs.
func TestRepoHookWrapperRunsWithTheLockFree(t *testing.T) {
	root := insideHooksRepo(t)
	runs := filepath.Join(t.TempDir(), "runs")
	holder := filepath.Join(root, ".git", "saddle-hooks", "lock.holder")
	if err := os.WriteFile(filepath.Join(root, "git/hooks/pre-commit"), []byte("#!/bin/sh\ncat "+holder+" >> "+runs+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "wrapper")
	s := lintgate.Shipped{Name: "pre-commit", Rel: "git/hooks/pre-commit", Kind: lintgate.KindHook, Source: "git/hooks/pre-commit"}
	if err := os.WriteFile(script, []byte(repoHookScript(s)), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(script)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "SADDLE_HOOK_LOCK_WAIT=1", "SADDLE_TRAIN=", "SADDLE_HOOK_DEPTH=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("wrapper: %v\n%s", err, out)
	}
	b, err := os.ReadFile(runs)
	if err != nil || !strings.Contains(string(b), "pre-commit hook") {
		t.Fatalf("hook didn't run under a recorded holder: %q, %v", b, err)
	}
}
