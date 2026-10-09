//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
)

// TestHarnessStopTmuxAwaitsPanes: a pane process still writing after the
// server is killed (an agent finishing a hook) is waited for, so stopTmux
// returns only once its last write has happened.
func TestHarnessStopTmuxAwaitsPanes(t *testing.T) {
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
