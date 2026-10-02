// Package gitx wraps the git CLI for worktrees, rebases and rename detection.
package gitx

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// calls counts git processes this package started, so tests can catch work
// that grows with history.
var calls atomic.Int64

// Calls returns how many git processes this package has started.
func Calls() int64 { return calls.Load() }

// Run executes git in dir and returns trimmed stdout. Errors carry stderr.
func Run(dir string, args ...string) (string, error) {
	calls.Add(1)
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// Root returns the main checkout of the repo containing dir, even from inside a worktree.
func Root(dir string) (string, error) {
	common, err := Run(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.Dir(common), nil
}

// Toplevel returns the worktree root containing dir.
func Toplevel(dir string) (string, error) {
	return Run(dir, "rev-parse", "--show-toplevel")
}

func RevParse(dir, ref string) (string, error) {
	return Run(dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
}

// ResolveCommits resolves each ref to the commit it names, in one git process.
// Refs that name no commit are left out of the map.
func ResolveCommits(dir string, refs []string) (map[string]string, error) {
	out := make(map[string]string, len(refs))
	if len(refs) == 0 {
		return out, nil
	}
	calls.Add(1)
	cmd := exec.Command("git", "-C", dir, "cat-file", "--batch-check=%(objectname)")
	var in, stdout, errb bytes.Buffer
	for _, r := range refs {
		in.WriteString(r + "^{commit}\n")
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = &in, &stdout, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git cat-file: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != len(refs) {
		return nil, fmt.Errorf("git cat-file: resolved %d of %d refs", len(lines), len(refs))
	}
	for i, l := range lines {
		// A ref that names nothing comes back as "<ref> missing" or "<ref> ambiguous".
		if !strings.ContainsRune(l, ' ') {
			out[refs[i]] = l
		}
	}
	return out, nil
}

func BranchExists(dir, branch string) bool {
	_, err := Run(dir, "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func CurrentBranch(dir string) (string, error) {
	return Run(dir, "symbolic-ref", "--quiet", "--short", "HEAD")
}

func WorktreeAdd(root, path, branch, base string) error {
	_, err := Run(root, "worktree", "add", "-b", branch, path, base)
	return err
}

func WorktreeRemove(root, path string) error {
	_, err := Run(root, "worktree", "remove", "--force", path)
	return err
}

// Dirty lists uncommitted changes (porcelain lines) in a worktree.
func Dirty(dir string) ([]string, error) {
	out, err := Run(dir, "status", "--porcelain")
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}

// CommitsBetween counts commits reachable from to but not from.
func CommitsBetween(dir, from, to string) (int, error) {
	out, err := Run(dir, "rev-list", "--count", from+".."+to)
	if err != nil {
		return 0, err
	}
	var n int
	_, err = fmt.Sscan(out, &n)
	return n, err
}

// RebaseResult describes a rebase attempt.
type RebaseResult struct {
	OK        bool
	Conflicts []string
	Output    string
	// Skipped counts commits left out because onto already held their work.
	Skipped int
}

// Rebase rebases the current branch of dir onto onto, with directory rename
// detection on so files added under a moved directory follow the move. On
// conflict it aborts when abort is true, leaving the worktree as it was.
func Rebase(dir, onto string, abort bool) (RebaseResult, error) {
	return RebaseOnto(dir, onto, "", abort)
}

// RebaseOnto is Rebase replaying only the commits after from (`git rebase
// --onto onto from`); an empty from replays everything onto lacks. A rebase
// that fails without conflicts is always aborted, so it never leaves the
// worktree mid-rebase with nothing for anyone to resolve.
func RebaseOnto(dir, onto, from string, abort bool) (RebaseResult, error) {
	args := []string{"-c", "merge.directoryRenames=true", "-c", "merge.renames=true",
		"-c", "rerere.enabled=true", "-c", "core.editor=true", "rebase"}
	if from != "" {
		args = append(args, "--onto", onto, from)
	} else {
		args = append(args, onto)
	}
	out, err := Run(dir, args...)
	if err == nil {
		return RebaseResult{OK: true, Output: out}, nil
	}
	conf, _ := Run(dir, "diff", "--name-only", "--diff-filter=U")
	if conf == "" {
		if RebaseInProgress(dir) {
			if _, aerr := Run(dir, "rebase", "--abort"); aerr != nil {
				return RebaseResult{Output: err.Error()}, fmt.Errorf("%w; and git rebase --abort failed: %v", err, aerr)
			}
		}
		return RebaseResult{Output: err.Error()}, err
	}
	res := RebaseResult{Conflicts: strings.Split(conf, "\n"), Output: err.Error()}
	if abort {
		if _, err := Run(dir, "rebase", "--abort"); err != nil {
			return res, err
		}
	}
	return res, nil
}

// UpdateRef moves a branch from old to new atomically (fails if it moved meanwhile).
func UpdateRef(dir, branch, new, old string) error {
	_, err := Run(dir, "update-ref", "refs/heads/"+branch, new, old)
	return err
}

type Rename struct{ Old, New string }

// Renames lists files renamed between two commits.
func Renames(dir, from, to string) ([]Rename, error) {
	out, err := Run(dir, "diff", "-M", "--name-status", "--diff-filter=R", from, to)
	if err != nil || out == "" {
		return nil, err
	}
	var rs []Rename
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 3 {
			rs = append(rs, Rename{Old: f[1], New: f[2]})
		}
	}
	return rs, nil
}

// ChangedFiles lists files changed between two commits.
func ChangedFiles(dir, from, to string) ([]string, error) {
	out, err := Run(dir, "diff", "--name-only", from, to)
	if err != nil || out == "" {
		return nil, err
	}
	return strings.Split(out, "\n"), nil
}
