package tmux

import (
	"strings"
	"testing"
)

// New windows and sessions carry saddle's TMPDIR and GOTMPDIR, so agents
// write their temp files to the scratch root, not the tmux server's /tmp
// (#322).
func TestNewWindowForwardsTempEnv(t *testing.T) {
	t.Setenv("TMPDIR", "/home/u/.cache/saddle/tmp")
	t.Setenv("GOTMPDIR", "/home/u/.cache/saddle/tmp")
	got := record(t)
	x := Tmux{Session: "s"}
	if _, err := x.NewWindow("w", "/d", "bash launch.sh"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.NewSession("w", "/d", "bash launch.sh"); err != nil {
		t.Fatal(err)
	}
	for _, c := range (*got)[:2] {
		if !strings.Contains(c, "-e TMPDIR=/home/u/.cache/saddle/tmp -e GOTMPDIR=/home/u/.cache/saddle/tmp bash launch.sh") {
			t.Errorf("temp env not forwarded: %s", c)
		}
	}
	t.Setenv("TMPDIR", "")
	t.Setenv("GOTMPDIR", "")
	*got = nil
	if _, err := x.NewWindow("w", "/d", "c"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains((*got)[0], "-e") {
		t.Errorf("unset vars forwarded: %s", (*got)[0])
	}
}
