package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// #663: a gate's tests start tmux servers and agents that leave its process
// group (setsid, daemonize), so killing the group missed them and they ran
// on for days. The gate reaps whatever is still alive under its gate-* dir
// once it ends, and removes the dir.
func TestRunGateEnvReapsWhatLeftItsGroup(t *testing.T) {
	if _, err := os.Stat("/proc/self/environ"); err != nil {
		t.Skip("no /proc")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid")
	}
	a, _ := setup(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	files := t.TempDir()
	pidf, dirf := filepath.Join(files, "daemon"), filepath.Join(files, "gatedir")
	cmd := "echo \"$TMPDIR\" > " + shellQuote(dirf) + "; " +
		"setsid sh -c 'echo $$ > " + pidf + "; exec sleep 600' </dev/null >/dev/null 2>&1 & " +
		"while [ ! -s " + shellQuote(pidf) + " ]; do sleep 0.01; done; echo gate-ok"
	res := a.RunGateEnv(context.Background(), "t1", ShellGate(a.Root, cmd, a.GateTimeout()))
	if res.Err != nil || !strings.Contains(res.Output, "gate-ok") {
		t.Fatalf("gate: %+v", res)
	}
	gone(t, pidf)
	b, err := os.ReadFile(dirf)
	must(t, err)
	dir := strings.TrimSpace(string(b))
	if !strings.HasPrefix(filepath.Base(dir), "gate-") {
		t.Fatalf("gate ran with TMPDIR %q, not a gate-* dir", dir)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("gate dir %s left behind (%v)", dir, err)
	}
}

// #663: saddle up sweeps gate-* dirs a killed gate left behind, with the
// processes running under them; a fresh gate dir and processes elsewhere
// are left alone.
func TestSweepStaleGatesKillsOnlyUnderStaleDirs(t *testing.T) {
	if _, err := os.Stat("/proc/self/cwd"); err != nil {
		t.Skip("no /proc")
	}
	a, _ := setup(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	legacy := filepath.Join(a.Root, ".saddle", "tmp")
	stale, fresh := filepath.Join(legacy, "gate-111"), filepath.Join(a.GateTmpdir(), "gate-222")
	elsewhere := t.TempDir()
	for _, d := range []string{filepath.Join(stale, "e2e-1"), fresh} {
		must(t, os.MkdirAll(d, 0o755))
	}
	inStale := daemon(t, filepath.Join(stale, "e2e-1"))
	argStale := daemon(t, elsewhere, filepath.Join(stale, "e2e-1", "launch.sh"))
	inFresh := daemon(t, fresh)
	outside := daemon(t, elsewhere)
	old := time.Now().Add(-2 * GateSweepAge)
	must(t, os.Chtimes(stale, old, old))

	res := a.SweepStaleGates(GateSweepAge)
	if len(res.Dirs) != 1 || res.Dirs[0] != stale || res.Killed < 2 {
		t.Fatalf("sweep = %+v, want %s removed and its 2 processes killed", res, stale)
	}
	for _, c := range []*exec.Cmd{inStale, argStale} {
		if c.Wait() == nil {
			t.Errorf("pid %d under the stale gate dir exited cleanly; it should have been killed", c.Process.Pid)
		}
	}
	for _, c := range []*exec.Cmd{inFresh, outside} {
		if !pidAlive(c.Process.Pid) {
			t.Errorf("pid %d outside the stale gate dir was killed", c.Process.Pid)
		}
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale gate dir left: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh gate dir removed: %v", err)
	}
}

// daemon starts sh -c sleep in dir in a session of its own, as a leaked
// tmux pane is; args only show up on its command line.
func daemon(t *testing.T, dir string, args ...string) *exec.Cmd {
	t.Helper()
	c := exec.Command("sh", append([]string{"-c", "sleep 600; :", "sh"}, args...)...)
	c.Dir = dir
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	must(t, c.Start())
	t.Cleanup(func() {
		_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		_ = c.Wait()
	})
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(5 * time.Millisecond) {
		if b, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(c.Process.Pid), "cmdline")); strings.HasPrefix(string(b), "sh\x00-c") {
			break
		}
	}
	return c
}
