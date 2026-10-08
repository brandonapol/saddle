package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/trust"
)

// TestMain isolates the package from where it runs. The merge train runs
// these tests from inside a saddle repo, as one of its agents: temp dirs
// under the repo would make saddleRoot find the real .saddle/config.toml,
// inherited GIT_* and SADDLE_* vars would point git and saddle at the real
// checkout, and the user's trust file would decide trust. So every test
// gets a temp dir outside any saddle repo, git stops looking above it, and
// trust decisions go to a scratch file.
func TestMain(m *testing.M) {
	os.Exit(runIsolated(m))
}

func runIsolated(m *testing.M) int {
	base, err := isolatedTemp()
	if err != nil {
		fmt.Fprintln(os.Stderr, "cli tests:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(base) }()
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch {
		case strings.HasPrefix(k, "SADDLE_"), k == "GIT_DIR", k == "GIT_WORK_TREE",
			k == "GIT_INDEX_FILE", k == "GIT_PREFIX", k == "GIT_COMMON_DIR", k == "CLAUDE_PROJECT_DIR":
			_ = os.Unsetenv(k)
		}
	}
	_ = os.Setenv("TMPDIR", base)
	_ = os.Setenv("GOTMPDIR", base) // t.TempDir prefers it to TMPDIR
	_ = os.Setenv("GIT_CEILING_DIRECTORIES", base)
	_ = os.Setenv(trust.EnvFile, filepath.Join(base, "trust.json"))
	return m.Run()
}

// isolatedTemp makes a temp dir with no saddle repo above it: under the
// system temp dir, else under the user cache dir.
func isolatedTemp() (string, error) {
	var parents []string
	parents = append(parents, os.TempDir())
	if c, err := os.UserCacheDir(); err == nil {
		parents = append(parents, filepath.Join(c, "saddle"))
	}
	for _, p := range parents {
		if err := os.MkdirAll(p, 0o755); err != nil {
			continue
		}
		d, err := os.MkdirTemp(p, "saddle-cli-test-")
		if err != nil {
			continue
		}
		if d, err = filepath.EvalSymlinks(d); err == nil && saddleRoot(d) == "" {
			return d, nil
		}
		_ = os.RemoveAll(d)
	}
	return "", fmt.Errorf("no temp dir outside a saddle repo under %v", parents)
}

// t.TempDir must have no saddle repo above it, wherever the train's gate
// points TMPDIR and GOTMPDIR (t.TempDir prefers GOTMPDIR).
func TestTempDirsAreOutsideSaddleRepos(t *testing.T) {
	if root := saddleRoot(t.TempDir()); root != "" {
		t.Fatalf("t.TempDir is inside the saddle repo %s", root)
	}
}
