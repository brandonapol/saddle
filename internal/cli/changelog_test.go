package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/release"
)

// fakeChangelogExec answers git and gh from a fixture: tag v0.1.0 made at
// noon on Oct 1, and four merged PRs, one before the tag.
func fakeChangelogExec(t *testing.T, top string, calls *[]string) {
	t.Helper()
	old := changelogExec
	changelogExec = func(_ string, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		*calls = append(*calls, call)
		switch {
		case strings.HasPrefix(call, "git rev-parse --show-toplevel"):
			return top, nil
		case strings.HasPrefix(call, "git describe"):
			return "v0.1.0", nil
		case strings.HasPrefix(call, "git log -1 --format=%cI v0.1.0"):
			return "2026-10-01T12:00:00Z", nil
		case strings.HasPrefix(call, "gh pr list"):
			return `[
 {"number": 5, "title": "feat: before the tag", "labels": [], "mergedAt": "2026-10-01T11:00:00Z"},
 {"number": 6, "title": "fix(store): keep claims", "labels": [], "mergedAt": "2026-10-02T00:00:00Z"},
 {"number": 7, "title": "Release workflow", "labels": [{"name": "enhancement"}], "mergedAt": "2026-10-03T00:00:00Z"},
 {"number": 8, "title": "Tidy footer 🤖 Generated with [Claude Code](https://claude.com/claude-code)", "labels": [], "mergedAt": "2026-10-04T00:00:00Z"}
]`, nil
		}
		t.Fatalf("unexpected call %q", call)
		return "", nil
	}
	t.Cleanup(func() { changelogExec = old })
}

func TestChangelogCmdSinceLastTag(t *testing.T) {
	top := t.TempDir()
	var calls []string
	fakeChangelogExec(t, top, &calls)
	out, _, err := runRoot(t, "changelog", "--version", "v0.2.0", "--date", "2026-10-10")
	if err != nil {
		t.Fatal(err)
	}
	want := "## v0.2.0 (2026-10-10)\n\n### Features\n\n- Release workflow (#7)\n\n### Fixes\n\n- keep claims (#6)\n\n### Other changes\n\n- Tidy footer (#8)\n"
	if out != want {
		t.Fatalf("changelog =\n%s\nwant\n%s", out, want)
	}

	if _, _, err := runRoot(t, "changelog", "--version", "v0.2.0", "--date", "2026-10-10", "--write"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(top, "CHANGELOG.md"))
	if err != nil || !strings.HasPrefix(string(b), release.Header) || !strings.Contains(string(b), want) {
		t.Fatalf("CHANGELOG.md = %q, %v", b, err)
	}
	if _, _, err := runRoot(t, "changelog", "--version", "v0.2.0", "--write"); err == nil {
		t.Fatal("--write added a second v0.2.0 section")
	}
	out, _, err = runRoot(t, "changelog", "--extract", "v0.2.0")
	if err != nil || !strings.HasPrefix(out, "### Features") || strings.Contains(out, "## v0.2.0") {
		t.Fatalf("--extract = %q, %v", out, err)
	}
	if _, _, err := runRoot(t, "changelog", "--extract", "v9.0.0"); err == nil {
		t.Fatal("--extract found a missing version")
	}
}

func TestChangelogCmdRejectsBadVersion(t *testing.T) {
	var calls []string
	fakeChangelogExec(t, t.TempDir(), &calls)
	if _, _, err := runRoot(t, "changelog", "--version", "0.2"); err == nil {
		t.Fatal("accepted a bad version")
	}
}
