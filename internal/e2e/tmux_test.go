//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// TestHarnessStopTmuxAwaitsPanes: a pane process still writing after the
// server is killed (an agent finishing a hook) is waited for, so stopTmux
// returns only once its last write has happened.
func TestHarnessStopTmuxAwaitsPanes(t *testing.T) {
	if Fork(t) {
		return
	}
	x := NewTmux(t)
	dir := t.TempDir()
	late, ready := filepath.Join(dir, "late"), filepath.Join(dir, "ready")
	// On the hangup it lingers, then writes: the write races TempDir removal
	// unless stopTmux waits for it.
	script := `trap 'sleep 0.5; echo late > "$1"; exit 0' HUP; : > "$2"; while :; do sleep 0.05; done`
	must(t, x.NewSession("linger", 40, 10, dir, "sh -c "+shq(script)+" sh "+shq(late)+" "+shq(ready)))
	Eventually(t, "the pane script to start", func() error {
		if _, err := os.Stat(ready); err != nil {
			return err
		}
		return nil
	})
	stopTmux(x)
	if _, err := os.Stat(late); err != nil {
		t.Fatalf("stopTmux returned before the pane process's last write: %v", err)
	}
	stopTmux(x) // a server that is already gone is a no-op
}

// TestHarnessTmuxSocketFitsUnderLongTempDir: under a deep TMPDIR (the
// train's make check runs in <scratch>/gate-*/e2e-*/), the socket path
// would pass the kernel's sun_path limit and tmux would fail with "File
// name too long". NewTmux falls back to a shorter dir.
func TestHarnessTmuxSocketFitsUnderLongTempDir(t *testing.T) {
	if Fork(t) {
		return
	}
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 90))
	must(t, os.MkdirAll(long, 0o755))
	t.Setenv("TMPDIR", long)
	x := NewTmux(t)
	if n := len(x.socket()); n > maxSocketPath {
		t.Fatalf("socket path is %d bytes, over %d: %s", n, maxSocketPath, x.socket())
	}
	must(t, x.NewSession("short", 40, 10, t.TempDir(), "sleep 30"))
}

// #663: a test's tmux server and a pane that ignores the hangup used to
// outlive the test (kill-server alone left the pane running, reparented to
// init). NewTmux's cleanup kills both, even when the test failed.
func TestHarnessTmuxCleanupKillsServerAndPanes(t *testing.T) {
	if Fork(t) {
		return
	}
	dir := t.TempDir()
	pidf := filepath.Join(dir, "pane")
	var server string
	t.Run("leaky", func(t *testing.T) {
		x := NewTmux(t)
		script := `trap '' HUP TERM; echo $$ > "$1"; while :; do sleep 0.05; done`
		must(t, x.NewSession("stubborn", 40, 10, dir, "sh -c "+shq(script)+" sh "+shq(pidf)))
		Eventually(t, "the pane script to start", func() error {
			if b, _ := os.ReadFile(pidf); len(b) == 0 {
				return errorf("no pid yet")
			}
			return nil
		})
		var err error
		server, err = x.Run("list-sessions", "-F", "#{pid}")
		must(t, err)
	})
	b, err := os.ReadFile(pidf)
	must(t, err)
	if pid := strings.TrimSpace(string(b)); alive(pid) {
		t.Errorf("the test's tmux pane %s outlived it", pid)
	}
	if alive(server) {
		t.Errorf("the test's tmux server %s outlived it", server)
	}
}

// #663: a journey go test's timeout killed never runs its cleanups. Fork
// reaps whatever it left under its temp dir, by temp dir, cwd or command
// line, and names it; a process elsewhere is left alone.
func TestHarnessReapJourneyKillsWhatAJourneyLeft(t *testing.T) {
	if Fork(t) {
		return
	}
	dir := filepath.Join(t.TempDir(), "j-1")
	must(t, os.MkdirAll(filepath.Join(dir, "demo"), 0o755))
	elsewhere := t.TempDir()
	byEnv := leak(t, elsewhere, "TMPDIR="+dir)          // a tmux server whose socket is in /run/user
	byCwd := leak(t, filepath.Join(dir, "demo"), "A=1") // a fake agent in its worktree
	outside := leak(t, elsewhere, "TMPDIR="+elsewhere)
	leaked := reapJourney(dir)
	if len(leaked) < 2 {
		t.Fatalf("reapJourney named %d leaked processes, want at least 2: %q", len(leaked), leaked)
	}
	for _, c := range []*exec.Cmd{byEnv, byCwd} {
		if c.Wait() == nil {
			t.Errorf("leaked pid %d exited cleanly; it should have been killed", c.Process.Pid)
		}
	}
	if !alive(strconv.Itoa(outside.Process.Pid)) {
		t.Error("a process outside the journey's dir was killed")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("journey dir left behind: %v", err)
	}
}

// leak starts a shell in dir in a session of its own, as tmux starts a
// server or pane, so no process group kill reaches it.
func leak(t *testing.T, dir string, env ...string) *exec.Cmd {
	t.Helper()
	c := exec.Command("sh", "-c", "sleep 600; :")
	c.Dir, c.Env = dir, append(os.Environ(), env...)
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	must(t, c.Start())
	t.Cleanup(func() {
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		_ = c.Wait()
	})
	Eventually(t, "the shell to start", func() error {
		if b, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(c.Process.Pid), "cmdline")); !strings.HasPrefix(string(b), "sh\x00") {
			return errorf("not exec'd yet")
		}
		return nil
	})
	return c
}

// alive reports whether pid names a live, non-zombie process.
func alive(pid string) bool {
	b, err := os.ReadFile(filepath.Join("/proc", strings.TrimSpace(pid), "stat"))
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i >= 0 && !strings.HasPrefix(strings.TrimSpace(s[i+1:]), "Z")
}
