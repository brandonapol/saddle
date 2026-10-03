package fakegh

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// git runs git in the bare repo dir with a fixed identity, so merges are
// reproducible and need no user config.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_DIR="+dir,
		"GIT_AUTHOR_NAME=GitHub", "GIT_AUTHOR_EMAIL=noreply@github.com",
		"GIT_COMMITTER_NAME=GitHub", "GIT_COMMITTER_EMAIL=noreply@github.com")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return strings.TrimSpace(out.String()), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func branchSHA(dir, branch string) string {
	sha, err := git(dir, "rev-parse", "--verify", "-q", "refs/heads/"+branch)
	if err != nil {
		return ""
	}
	return sha
}

// errConflict means a merge can't be made without a conflict.
var errConflict = errors.New("merge conflict")

// mergeTree merges theirs into ours (optionally from base) and returns the
// tree, or errConflict.
func mergeTree(dir, ours, theirs, base string) (string, error) {
	args := []string{"merge-tree", "--write-tree", "--no-messages"}
	if base != "" {
		args = append(args, "--merge-base="+base)
	}
	out, err := git(dir, append(args, ours, theirs)...)
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 {
			return "", errConflict
		}
		return "", err
	}
	tree, _, _ := strings.Cut(out, "\n")
	return tree, nil
}

// mergeable computes GitHub's mergeable for p from git.
func mergeable(dir string, p *PR) string {
	head, base := branchSHA(dir, p.Head), branchSHA(dir, p.Base)
	if head == "" || base == "" {
		return "UNKNOWN"
	}
	if _, err := mergeTree(dir, base, head, ""); err != nil {
		return "CONFLICTING"
	}
	return "MERGEABLE"
}

// merge merges p into its base with method and records it, as GitHub does.
// head, if set, must match the PR's head (--match-head-commit).
func merge(dir string, s *State, p *PR, method, head string) error {
	if p.State != "OPEN" {
		return fmt.Errorf("pull request #%d is not open (%s)", p.Number, strings.ToLower(p.State))
	}
	headSHA, baseSHA := branchSHA(dir, p.Head), branchSHA(dir, p.Base)
	if headSHA == "" || baseSHA == "" {
		return fmt.Errorf("pull request #%d: head %s or base %s is missing", p.Number, p.Head, p.Base)
	}
	if head != "" && head != headSHA {
		return fmt.Errorf("head branch was modified. Review and try the merge again (head is %s, expected %s)", headSHA, head)
	}
	if p.Mergeable == "CONFLICTING" {
		return fmt.Errorf("pull request #%d is not mergeable: the merge commit cannot be cleanly created", p.Number)
	}
	var tip string
	var err error
	switch method {
	case "squash":
		if !s.Repo.AllowSquash {
			return errors.New("squash merges are not allowed on this repository")
		}
		tip, err = squash(dir, p, baseSHA, headSHA)
	case "rebase":
		if !s.Repo.AllowRebase {
			return errors.New("rebase merges are not allowed on this repository")
		}
		tip, err = rebase(dir, baseSHA, headSHA)
	case "merge":
		if !s.Repo.AllowMerge {
			return errors.New("merge commits are not allowed on this repository")
		}
		tip, err = mergeCommit(dir, p, baseSHA, headSHA)
	default:
		return fmt.Errorf("unknown merge method %q", method)
	}
	if errors.Is(err, errConflict) {
		return fmt.Errorf("pull request #%d is not mergeable: the merge commit cannot be cleanly created", p.Number)
	}
	if err != nil {
		return err
	}
	if _, err := git(dir, "update-ref", "refs/heads/"+p.Base, tip, baseSHA); err != nil {
		return err
	}
	p.State, p.HeadOid, p.MergeCommit, p.MergedBy = "MERGED", headSHA, tip, method
	if s.Repo.DeleteBranch {
		for _, o := range s.Open() {
			if o.Base == p.Head {
				o.Base = p.Base
			}
		}
		if _, err := git(dir, "update-ref", "-d", "refs/heads/"+p.Head); err != nil {
			return err
		}
	}
	return nil
}

func squash(dir string, p *PR, base, head string) (string, error) {
	tree, err := mergeTree(dir, base, head, "")
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("%s (#%d)\n\n%s", p.Title, p.Number, p.Body)
	return git(dir, "commit-tree", tree, "-p", base, "-m", msg)
}

func mergeCommit(dir string, p *PR, base, head string) (string, error) {
	tree, err := mergeTree(dir, base, head, "")
	if err != nil {
		return "", err
	}
	msg := fmt.Sprintf("Merge pull request #%d from %s\n\n%s", p.Number, p.Head, p.Title)
	return git(dir, "commit-tree", tree, "-p", base, "-p", head, "-m", msg)
}

// rebase replays head's own commits onto base, one new commit each, keeping
// their messages, as GitHub's rebase merge does.
func rebase(dir, base, head string) (string, error) {
	mb, err := git(dir, "merge-base", base, head)
	if err != nil {
		return "", err
	}
	list, err := git(dir, "rev-list", "--reverse", "--no-merges", mb+".."+head)
	if err != nil {
		return "", err
	}
	tip := base
	for _, c := range strings.Fields(list) {
		tree, err := mergeTree(dir, tip, c, c+"^")
		if err != nil {
			return "", err
		}
		msg, err := git(dir, "log", "-1", "--format=%B", c)
		if err != nil {
			return "", err
		}
		if tip, err = git(dir, "commit-tree", tree, "-p", tip, "-m", msg); err != nil {
			return "", err
		}
	}
	return tip, nil
}

// changedFiles lists the files p changes against its base.
func changedFiles(dir string, p *PR) []string {
	head, base := branchSHA(dir, p.Head), branchSHA(dir, p.Base)
	if p.State != "OPEN" {
		head = p.HeadOid
	}
	if head == "" || base == "" {
		return nil
	}
	out, err := git(dir, "diff", "--name-only", base+"..."+head)
	if err != nil || out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}
