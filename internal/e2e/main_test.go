//go:build e2e

package e2e

import (
	"fmt"
	"testing"
)

var bins Bins

// TestMain builds the binaries once and runs each journey in its own
// process, in parallel (see Main and Fork).
func TestMain(m *testing.M) { Main(m, &bins) }

// world builds a World for t and logs diagnostics if t fails.
func world(t *testing.T, opts Options) *World {
	t.Helper()
	w := New(t, bins, opts)
	t.Cleanup(w.Diagnose)
	return w
}

// errorf is fmt.Errorf, for conditions.
func errorf(format string, a ...any) error { return fmt.Errorf(format, a...) }

// TestHarnessTempDirsAreOutsideSaddleRepos: a World's repo must have no
// saddle repo above it, wherever TMPDIR and GOTMPDIR point (the train once
// ran the gate under .saddle/tmp): the plugin walks up for
// .saddle/config.toml and would take the real checkout for the World's.
func TestHarnessTempDirsAreOutsideSaddleRepos(t *testing.T) {
	if Fork(t) {
		return
	}
	if root := EnclosingSaddleRepo(t.TempDir()); root != "" {
		t.Fatalf("t.TempDir is inside the saddle repo %s", root)
	}
}
