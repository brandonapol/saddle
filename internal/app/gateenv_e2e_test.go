package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// #692: the sweep finds leaked e2e processes by their SADDLE_E2E_DIR tag, so
// one whose dir is already gone is reaped, a live one is spared, and an
// e2etmux socket dir with no server left is removed.
func TestSweepStaleGatesReapsTaggedE2EProcesses(t *testing.T) {
	if _, err := os.Stat("/proc/self/cwd"); err != nil {
		t.Skip("no /proc")
	}
	a, _ := setup(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	run := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", run)
	live := t.TempDir()
	gone := filepath.Join(t.TempDir(), "deleted")
	tagged := func(dir string) *exec.Cmd {
		c := exec.Command("sh", "-c", "sleep 600; :")
		c.Env = append(os.Environ(), "SADDLE_E2E_DIR="+dir)
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		must(t, c.Start())
		t.Cleanup(func() { _ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL); _ = c.Wait() })
		return c
	}
	leaked, spared := tagged(gone), tagged(live)
	time.Sleep(100 * time.Millisecond)

	orphan := filepath.Join(run, "e2etmux111")
	must(t, os.MkdirAll(orphan, 0o755))
	old := time.Now().Add(-time.Hour)
	must(t, os.Chtimes(orphan, old, old))
	inUse := filepath.Join(run, "e2etmux222")
	must(t, os.MkdirAll(inUse, 0o755))
	must(t, os.Chtimes(inUse, old, old))
	server := daemon(t, t.TempDir(), "-S", filepath.Join(inUse, "tmux-1", "default"))

	res := a.SweepStaleGates(GateSweepAge)
	if res.E2E < 1 {
		t.Fatalf("sweep = %+v, want the leaked process counted", res)
	}
	if leaked.Wait() == nil {
		t.Error("tagged process with a deleted dir exited cleanly; it should have been killed")
	}
	if !pidAlive(spared.Process.Pid) || !pidAlive(server.Process.Pid) {
		t.Error("a live e2e process was killed")
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Errorf("e2etmux dir with no server left: %v", err)
	}
	if _, err := os.Stat(inUse); err != nil {
		t.Errorf("e2etmux dir with a live server removed: %v", err)
	}
}
