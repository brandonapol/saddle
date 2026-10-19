package procreap

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// #692: a process tagged with SADDLE_E2E_DIR is found by its tag, not by
// where its dir is: a leaked one whose dir was deleted is reaped, one whose
// dir is live and fresh is spared.
func TestReapTaggedKillsGoneAndStaleSparesLive(t *testing.T) {
	needProc(t)
	root := t.TempDir()
	live, stale, gone := filepath.Join(root, "live"), filepath.Join(root, "stale"), filepath.Join(root, "gone")
	for _, d := range []string{live, stale} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	elsewhere := t.TempDir()
	tag := func(d string) []string { return []string{EnvE2EDir + "=" + d} }
	cGone := start(t, elsewhere, tag(gone))
	cStale := start(t, elsewhere, tag(stale))
	cLive := start(t, elsewhere, tag(live))
	untagged := start(t, elsewhere, nil)

	cutoff := time.Now().Add(-time.Hour)
	if got := FindTagged(cutoff); !has(got, cGone, cStale) || has(got, cLive, untagged) {
		t.Fatalf("FindTagged = %v; want %d and %d, not %d or %d", got, cGone.Process.Pid, cStale.Process.Pid, cLive.Process.Pid, untagged.Process.Pid)
	}
	if n := ReapTagged(cutoff, time.Second); n < 2 {
		t.Fatalf("ReapTagged signalled %d, want at least 2", n)
	}
	for _, c := range []*exec.Cmd{cGone, cStale} {
		if c.Wait() == nil {
			t.Error("a leaked tagged process exited cleanly; it should have been killed")
		}
	}
	for _, p := range []int{cLive.Process.Pid, untagged.Process.Pid} {
		if syscall.Kill(p, 0) != nil {
			t.Errorf("pid %d was killed; it should be spared", p)
		}
	}
}
