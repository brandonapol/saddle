package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
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
