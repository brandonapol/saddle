package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, dir, "add", "-A")
	gitT(t, dir, "commit", "-qm", name)
}

// LandedPrefix covers only the own commits onto holds: none when onto has
// only unrelated work, and the first of two when only it was squash-landed.
func TestLandedPrefix(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	commitFile(t, dir, "base.txt", "base\n")
	gitT(t, dir, "checkout", "-qb", "task")
	commitFile(t, dir, "a.txt", "1\n")
	commitFile(t, dir, "a.txt", "1\n2\n")
	first := gitT(t, dir, "rev-parse", "HEAD")
	commitFile(t, dir, "b.txt", "b\n")
	gitT(t, dir, "checkout", "-q", "main")
	commitFile(t, dir, "other.txt", "other\n")
	gitT(t, dir, "checkout", "-q", "task")

	if c, n, err := LandedPrefix(dir, "main"); err != nil || c != "" || n != 0 {
		t.Fatalf("unrelated onto: LandedPrefix = %q, %d, %v; want nothing", c, n, err)
	}

	gitT(t, dir, "checkout", "-q", "main")
	commitFile(t, dir, "a.txt", "1\n2\n") // a1 and a2 squashed
	gitT(t, dir, "checkout", "-q", "task")
	if c, n, err := LandedPrefix(dir, "main"); err != nil || c != first || n != 2 {
		t.Fatalf("squashed onto: LandedPrefix = %q, %d, %v; want %s, 2", c, n, err, first)
	}
}
