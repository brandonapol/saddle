package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/forge"
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
		}
		t.Fatalf("unexpected call %q", call)
		return "", nil
	}
	day := func(d, h int) time.Time { return time.Date(2026, 10, d, h, 0, 0, 0, time.UTC) }
	fakeChangelogMerged(t, func(base string) ([]forge.MergedPR, error) {
		*calls = append(*calls, "merged "+base)
		return []forge.MergedPR{
			{Number: 5, Title: "feat: before the tag", MergedAt: day(1, 11)},
			{Number: 6, Title: "fix(store): keep claims", MergedAt: day(2, 0)},
			{Number: 7, Title: "Release workflow", Labels: []string{"enhancement"}, MergedAt: day(3, 0)},
			{Number: 8, Title: "Tidy footer 🤖 Generated with [Claude Code](https://claude.com/claude-code)", MergedAt: day(4, 0)},
		}, nil
	})
	t.Cleanup(func() { changelogExec = old })
}

// fakeChangelogMerged answers the forge's merged-PR list.
func fakeChangelogMerged(t *testing.T, list func(base string) ([]forge.MergedPR, error)) {
	t.Helper()
	old := changelogMerged
	changelogMerged = func(_ context.Context, _, base string) ([]forge.MergedPR, error) { return list(base) }
	t.Cleanup(func() { changelogMerged = old })
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

// A forge that can't list merged PRs (GitLab, local-only) gets a clear
// refusal instead of a gh error (#648).
func TestChangelogCmdUnsupportedForge(t *testing.T) {
	var calls []string
	fakeChangelogExec(t, t.TempDir(), &calls)
	fakeChangelogMerged(t, func(string) ([]forge.MergedPR, error) {
		return nil, fmt.Errorf("list merged PRs: %w", forge.ErrUnsupported)
	})
	_, _, err := runRoot(t, "changelog", "--version", "v0.2.0")
	if err == nil || !strings.Contains(err.Error(), "needs a GitHub forge") {
		t.Fatalf("err = %v, want the GitHub-only refusal", err)
	}
}
