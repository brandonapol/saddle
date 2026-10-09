//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

var bins Bins

func TestMain(m *testing.M) {
	// The train runs this from inside a saddle repo: temp dirs under it
	// would put every World's repo inside the real one.
	base, err := isolatedTemp()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e:", err)
		os.Exit(1)
	}
	_ = os.Setenv("TMPDIR", base)
	_ = os.Setenv("GOTMPDIR", base) // t.TempDir prefers it to TMPDIR
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
	_ = os.RemoveAll(base)
	os.Exit(code)
}

// isolatedTemp makes a temp dir with no saddle repo above it: under the
// temp dir the run was given, else the system's, else the user cache dir.
func isolatedTemp() (string, error) {
	parents := []string{os.TempDir(), "/tmp"}
	if c, err := os.UserCacheDir(); err == nil {
		parents = append(parents, filepath.Join(c, "saddle"))
	}
	for _, p := range parents {
		if os.MkdirAll(p, 0o755) != nil {
			continue
		}
		d, err := os.MkdirTemp(p, "e2e-")
		if err != nil {
			continue
		}
		if d, err = filepath.EvalSymlinks(d); err == nil && enclosingSaddleRepo(d) == "" {
			return d, nil
		}
		_ = os.RemoveAll(d)
	}
	return "", fmt.Errorf("no temp dir outside a saddle repo under %v", parents)
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

// enclosingSaddleRepo is the nearest dir at or above dir with a
// .saddle/config.toml, or "": what the plugin's root lookup would find.
func enclosingSaddleRepo(dir string) string {
	for dir = filepath.Clean(dir); ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, ".saddle", "config.toml")); err == nil {
			return dir
		}
		if dir == filepath.Dir(dir) {
			return ""
		}
	}
}

// TestHarnessTempDirsAreOutsideSaddleRepos: a World's repo must have no
// saddle repo above it, wherever TMPDIR and GOTMPDIR point (the train once
// ran the gate under .saddle/tmp): the plugin walks up for
// .saddle/config.toml and would take the real checkout for the World's.
func TestHarnessTempDirsAreOutsideSaddleRepos(t *testing.T) {
	if root := enclosingSaddleRepo(t.TempDir()); root != "" {
		t.Fatalf("t.TempDir is inside the saddle repo %s", root)
	}
}
