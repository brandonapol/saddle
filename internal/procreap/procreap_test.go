package procreap

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func needProc(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("/proc/self/cwd"); err != nil {
		t.Skip("no /proc")
	}
}

// start runs sh -c sleep in its own session, as tmux and setsid'd agents are, so
// the test's process group can't reach it; t.Cleanup kills it regardless.
func start(t *testing.T, dir string, env []string, args ...string) *exec.Cmd {
	t.Helper()
	c := exec.Command("sh", append([]string{"-c", "sleep 60; :", "sh"}, args...)...)
	c.Dir = dir
	c.Env = append(os.Environ(), env...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		_ = c.Wait()
	})
	// Wait until the exec has happened, so /proc shows sh, not the fork.
	for range 200 {
		if b, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(c.Process.Pid), "cmdline")); strings.HasPrefix(string(b), "sh\x00-c") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	return c
}

// #663: what a gate leaves under its dir, by cwd, by argument or by its
// temp dir, is found and killed; a process outside the dir is not.
func TestReapKillsOnlyWhatIsUnderDir(t *testing.T) {
	needProc(t)
	root := t.TempDir()
	gate := filepath.Join(root, "gate-1")
	sub := filepath.Join(gate, "e2e-1", "demo")
	other := filepath.Join(root, "gate-10") // a sibling sharing the prefix
	for _, d := range []string{sub, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	byCwd := start(t, sub, nil)
	byArg := start(t, other, nil, "--script-dir="+filepath.Join(gate, "scripts"))
	byEnv := start(t, other, []string{"TMPDIR=" + gate})
	outside := start(t, other, []string{"TMPDIR=" + other})

	// A shell's sleep child is found too when it shares the cwd or env.
	noEnv := Find(gate, Match{})
	if !has(noEnv, byCwd, byArg) || has(noEnv, byEnv) || has(noEnv, outside) {
		t.Fatalf("Find without Env = %v; want %d and %d, not %d or %d", noEnv, byCwd.Process.Pid, byArg.Process.Pid, byEnv.Process.Pid, outside.Process.Pid)
	}
	withEnv := Find(gate, Match{Env: true})
	if !has(withEnv, byCwd, byArg, byEnv) || has(withEnv, outside) {
		t.Fatalf("Find with Env = %v; want %d, %d and %d, not %d", withEnv, byCwd.Process.Pid, byArg.Process.Pid, byEnv.Process.Pid, outside.Process.Pid)
	}
	if n := Reap(gate, time.Second, Match{Env: true}); n < 3 {
		t.Fatalf("Reap signalled %d, want at least 3", n)
	}
	for _, c := range []*exec.Cmd{byCwd, byArg, byEnv} {
		if err := c.Wait(); err == nil {
			t.Errorf("pid %d exited cleanly; it should have been killed", c.Process.Pid)
		}
	}
	if err := outside.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("a process outside the dir was killed: %v", err)
	}
	if got := Find(gate, Match{Env: true}); len(got) != 0 {
		t.Fatalf("still under the dir after Reap: %v", got)
	}
}

func TestFindRefusesBroadDirs(t *testing.T) {
	needProc(t)
	home, _ := os.UserHomeDir()
	for _, d := range []string{"", "relative/dir", "/", "/tmp", home} {
		if got := Find(d, Match{Env: true}); got != nil {
			t.Errorf("Find(%q) = %v, want nothing", d, got)
		}
	}
}

func TestFindSkipsSelfAndAncestors(t *testing.T) {
	needProc(t)
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range Find(wd, Match{Env: true}) {
		if ancestors()[p] {
			t.Fatalf("Find listed this process or an ancestor: %d", p)
		}
	}
}

// has reports whether pids holds every one of cs.
func has(pids []int, cs ...*exec.Cmd) bool {
	for _, c := range cs {
		if !slices.Contains(pids, c.Process.Pid) {
			return false
		}
	}
	return true
}
