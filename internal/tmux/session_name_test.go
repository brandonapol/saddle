package tmux

import (
	"os/exec"
	"testing"

	"github.com/brandonapol/saddle/internal/config"
)

// A session named from a dotted repo dir must be findable by has-session.
func TestSanitizedSessionIsTargetable(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	name := config.DefaultSession("/x/t93-test.repo:x")
	tm := Tmux{Session: name}
	if _, err := tm.NewSession("w", t.TempDir(), "sleep 30"); err != nil {
		t.Skipf("cannot start tmux: %v", err)
	}
	t.Cleanup(func() { _ = tm.KillSession() })
	if !tm.HasSession() {
		t.Fatalf("has-session -t =%s failed", name)
	}
}
