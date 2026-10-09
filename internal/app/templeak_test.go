package app

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The test binary starts once per go test run and once per ref update, as
// the ref guard hook Init installs. Neither start may leave anything in the
// temp dir: a leaked dir per start filled /tmp with ~437k empty
// saddle-app-runq-* dirs.
func TestTestBinaryLeavesNoTempDirs(t *testing.T) {
	t.Parallel()
	for name, args := range map[string][]string{
		"tests":    {"-test.run=^$"},
		"refguard": {"refguard", "x"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			cmd := exec.Command(os.Args[0], args...)
			cmd.Env = append(os.Environ(), "TMPDIR="+tmp)
			cmd.Stdin = strings.NewReader("")
			_ = cmd.Run() // the hook may refuse x; only what it leaves matters
			ents, err := os.ReadDir(tmp)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range ents {
				t.Errorf("left behind %s", e.Name())
			}
		})
	}
}
