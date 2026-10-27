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

// #147: the task was cut from integration holding a1 and a2, which later
// landed on main as one squash a reviewer edited, so neither tree nor
// patch-id matches. Integration's fork point still marks them as not the
// task's own. Without the reflog ReplayFrom falls back to LandedPrefix.
func TestReplayFrom(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	commitFile(t, dir, "base.txt", "base\n")
	gitT(t, dir, "checkout", "-qb", "integ")
	commitFile(t, dir, "a.txt", "1\n")
	commitFile(t, dir, "a.txt", "1\n2\n")
	cut := gitT(t, dir, "rev-parse", "HEAD")
	gitT(t, dir, "checkout", "-qb", "task")
	commitFile(t, dir, "b.txt", "b\n")
	gitT(t, dir, "checkout", "-q", "main")
	commitFile(t, dir, "a.txt", "1\n2\nreviewed\n") // the edited squash
	gitT(t, dir, "update-ref", "refs/heads/integ", "main")
	gitT(t, dir, "checkout", "-q", "task")

	if c, n, err := LandedPrefix(dir, "integ"); err != nil || c != "" || n != 0 {
		t.Fatalf("LandedPrefix = %q, %d, %v; want nothing (the squash was edited)", c, n, err)
	}
	if c, n, err := ReplayFrom(dir, "integ", ""); err != nil || c != "" || n != 0 {
		t.Fatalf("untracked ReplayFrom = %q, %d, %v; want nothing", c, n, err)
	}
	if c, n, err := ReplayFrom(dir, "integ", "integ"); err != nil || c != cut || n != 2 {
		t.Fatalf("ReplayFrom = %q, %d, %v; want %s, 2", c, n, err, cut)
	}

	// Still on integration: the fork point adds nothing to a plain rebase.
	gitT(t, dir, "update-ref", "refs/heads/integ", cut)
	if c, n, err := ReplayFrom(dir, "integ", "integ"); err != nil || c != "" || n != 0 {
		t.Fatalf("on integ: ReplayFrom = %q, %d, %v; want nothing", c, n, err)
	}

	// A fresh clone or an expired reflog has no fork point.
	gitT(t, dir, "update-ref", "refs/heads/integ", "main")
	gitT(t, dir, "reflog", "expire", "--expire=now", "--expire-unreachable=now", "refs/heads/integ")
	if c, n, err := ReplayFrom(dir, "integ", "integ"); err != nil || c != "" || n != 0 {
		t.Fatalf("no reflog: ReplayFrom = %q, %d, %v; want LandedPrefix's nothing", c, n, err)
	}
}

// #643: integration once pointed at the task's base, then a restack dropped
// that work without landing it. The fork point still names it, but onto holds
// none of its content, so ReplayFrom must replay it rather than skip it.
func TestReplayFromKeepsRemovedWork(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	commitFile(t, dir, "base.txt", "base\n")
	gitT(t, dir, "checkout", "-qb", "integ")
	commitFile(t, dir, "x.txt", "x\n")
	gitT(t, dir, "checkout", "-qb", "task")
	commitFile(t, dir, "b.txt", "b\n")
	gitT(t, dir, "checkout", "-q", "main")
	commitFile(t, dir, "other.txt", "other\n")
	gitT(t, dir, "update-ref", "refs/heads/integ", "main") // x.txt dropped, not landed
	gitT(t, dir, "checkout", "-q", "task")

	if c, n, err := ReplayFrom(dir, "integ", "integ"); err != nil || c != "" || n != 0 {
		t.Fatalf("ReplayFrom = %q, %d, %v; want a plain rebase (nothing skipped)", c, n, err)
	}
}

// #742: integration once held the task's base x, which edits f.txt, then a
// restack dropped x without landing it while integration gained unrelated
// edits to f.txt. Touching the same file is not holding x's change, so
// ReplayFrom must replay x rather than skip it.
func TestReplayFromKeepsWorkOnSharedFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	const base = "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\n"
	for _, tc := range []struct{ name, mine, theirs string }{
		// integration edits another hunk of the same file.
		{"other hunk", "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nx\n", "y\na\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\n"},
		// integration adds the same-looking line somewhere else.
		{"same line elsewhere", "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nx\n", "x\na\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			gitT(t, dir, "init", "-q", "-b", "main")
			// RebaseOnto commits through gitx.Run, which does not get gitT's
			// author env. CI has no global identity, so set one in the repo.
			gitT(t, dir, "config", "user.name", "t")
			gitT(t, dir, "config", "user.email", "t@example.com")
			commitFile(t, dir, "f.txt", base)
			gitT(t, dir, "checkout", "-qb", "integ")
			commitFile(t, dir, "f.txt", tc.mine)
			gitT(t, dir, "checkout", "-qb", "task")
			commitFile(t, dir, "b.txt", "b\n")
			gitT(t, dir, "checkout", "-q", "main")
			commitFile(t, dir, "f.txt", tc.theirs)
			gitT(t, dir, "update-ref", "refs/heads/integ", "main") // x dropped, not landed
			gitT(t, dir, "checkout", "-q", "task")

			c, n, err := ReplayFrom(dir, "integ", "integ")
			if err != nil || c != "" || n != 0 {
				t.Fatalf("ReplayFrom = %q, %d, %v; want a plain rebase (nothing skipped)", c, n, err)
			}
			if _, err := RebaseOnto(dir, "integ", c, false); err != nil {
				t.Fatal(err)
			}
			if got := gitT(t, dir, "show", "HEAD:f.txt"); !strings.HasSuffix(got, "l\nx") {
				t.Fatalf("f.txt after rebase = %q; x's line was dropped", got)
			}
		})
	}
}

// Content integration genuinely holds is still skipped: a cherry-picked
// duplicate, and a squash a reviewer edited in another hunk.
func TestReplayFromSkipsHeldWork(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	const base = "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\n"
	const mine = "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nx\n"
	for _, tc := range []struct{ name, theirs string }{
		{"duplicate", mine},
		{"edited squash", "reviewed\na\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\nx\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			gitT(t, dir, "init", "-q", "-b", "main")
			commitFile(t, dir, "f.txt", base)
			gitT(t, dir, "checkout", "-qb", "integ")
			commitFile(t, dir, "f.txt", mine)
			cut := gitT(t, dir, "rev-parse", "HEAD")
			gitT(t, dir, "checkout", "-qb", "task")
			commitFile(t, dir, "b.txt", "b\n")
			gitT(t, dir, "checkout", "-q", "main")
			if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(tc.theirs), 0o644); err != nil {
				t.Fatal(err)
			}
			gitT(t, dir, "commit", "-qam", "squash")
			gitT(t, dir, "update-ref", "refs/heads/integ", "main")
			gitT(t, dir, "checkout", "-q", "task")

			if c, n, err := ReplayFrom(dir, "integ", "integ"); err != nil || c != cut || n != 1 {
				t.Fatalf("ReplayFrom = %q, %d, %v; want %s, 1", c, n, err, cut)
			}
		})
	}
}
