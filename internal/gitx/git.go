// Package gitx wraps the git CLI for worktrees, rebases and rename detection.
package gitx

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Run executes git in dir and returns trimmed stdout. Errors carry stderr.
func Run(dir string, args ...string) (string, error) {
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
}

// Rebase rebases the current branch of dir onto onto, with directory rename
// detection on so files added under a moved directory follow the move. On
// conflict it aborts when abort is true, leaving the worktree as it was.
func Rebase(dir, onto string, abort bool) (RebaseResult, error) {
	out, err := Run(dir, "-c", "merge.directoryRenames=true", "-c", "merge.renames=true",
		"-c", "rerere.enabled=true", "-c", "core.editor=true", "rebase", onto)
	if err == nil {
		return RebaseResult{OK: true, Output: out}, nil
	}
	conf, _ := Run(dir, "diff", "--name-only", "--diff-filter=U")
	if conf == "" {
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
