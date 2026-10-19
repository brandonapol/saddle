package e2e

import (
	"os"
	"slices"
	"testing"

	"github.com/brandonapol/saddle/internal/procreap"
)

// #692: the tmux server (and so its panes and fake agents) is tagged with
// the test's dir, whether the env is os.Environ or a world's own list.
func TestTmuxEnvCarriesE2ETag(t *testing.T) {
	if Fork(t) {
		return
	}
	t.Setenv(procreap.EnvE2EDir, "")
	os.Unsetenv(procreap.EnvE2EDir)
	x := &Tmux{Dir: "/x/e2etmux1"}
	if !slices.Contains(x.env(), procreap.EnvE2EDir+"=/x/e2etmux1") {
		t.Errorf("env = %v, want the tag naming the socket dir", x.env())
	}
	x.Env = []string{"A=b", procreap.EnvE2EDir + "=/x/j-1"}
	if e := x.env(); !slices.Contains(e, procreap.EnvE2EDir+"=/x/j-1") || slices.Contains(e, procreap.EnvE2EDir+"=/x/e2etmux1") {
		t.Errorf("env = %v, want the test's own tag kept", e)
	}
}
