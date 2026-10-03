//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"testing"
)

var bins Bins

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "e2ebin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if bins, err = Build(dir); err != nil {
		_ = os.RemoveAll(dir)
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// In-process saddle code (the auto-merge tick) runs gh from PATH too.
	_ = os.Setenv("PATH", bins.Dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, k := range []string{"TMUX", "SADDLE_ROOT", "SADDLE_TASK", "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE"} {
		_ = os.Unsetenv(k)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// world builds a World for t and logs diagnostics if t fails.
func world(t *testing.T, opts Options) *World {
	t.Helper()
	w := New(t, bins, opts)
	t.Cleanup(w.Diagnose)
	return w
}

// errorf is fmt.Errorf, for conditions.
func errorf(format string, a ...any) error { return fmt.Errorf(format, a...) }
