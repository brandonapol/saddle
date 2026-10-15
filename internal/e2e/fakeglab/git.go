package fakeglab

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// git runs git in the bare repo dir with a fixed identity.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_DIR="+dir,
		"GIT_AUTHOR_NAME=GitLab", "GIT_AUTHOR_EMAIL=noreply@gitlab.com",
		"GIT_COMMITTER_NAME=GitLab", "GIT_COMMITTER_EMAIL=noreply@gitlab.com")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
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

var errConflict = errors.New("merge conflict")

// mergeTree merges theirs into ours and returns the tree, or errConflict.
func mergeTree(dir, ours, theirs string) (string, error) {
	out, err := git(dir, "merge-tree", "--write-tree", "--no-messages", ours, theirs)
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

func isAncestor(dir, a, b string) bool {
	_, err := git(dir, "merge-base", "--is-ancestor", a, b)
	return err == nil
}

// mergeStatus is GitLab's detailed_merge_status for m, and whether it has
// conflicts.
func mergeStatus(dir string, s *State, m *MR) (string, bool) {
	if m.State != "opened" {
		return "not_open", false
	}
	head, base := branchSHA(dir, m.Source), branchSHA(dir, m.Target)
	if head == "" || base == "" {
		return "checking", false
	}
	if _, err := mergeTree(dir, base, head); err != nil {
		return "conflict", true
	}
	switch {
	case strings.HasPrefix(m.Title, "Draft:"):
		return "draft_status", false
	case s.MergeMethod == "ff" && !isAncestor(dir, base, head):
		return "need_rebase", false
	case s.PipelineMustSucceed && m.Pipeline != nil && m.Pipeline.status() == Failed:
		return "ci_must_pass", false
	case s.PipelineMustSucceed && m.Pipeline != nil && m.Pipeline.status() != Success:
		return "ci_still_running", false
	}
	return "mergeable", false
}

// httpError is a REST failure: its status and message.
type httpError struct {
	code int
	msg  string
}

func (e *httpError) Error() string { return fmt.Sprintf("%d %s", e.code, e.msg) }

// merge merges m into its target as GitLab does: a squash commit, or the
// head itself on a fast-forward project, else a merge commit. head, if
// set, must be the MR's head (sha).
func merge(dir string, s *State, m *MR, squash bool, head string) error {
	if m.State != "opened" {
		return &httpError{405, "Method Not Allowed"}
	}
	headSHA, baseSHA := branchSHA(dir, m.Source), branchSHA(dir, m.Target)
	if headSHA == "" || baseSHA == "" {
		return &httpError{422, "source or target branch is missing"}
	}
	if head != "" && head != headSHA {
		return &httpError{409, "SHA does not match HEAD of source branch: " + headSHA}
	}
	status, _ := mergeStatus(dir, s, m)
	switch status {
	case "conflict", "need_rebase":
		return &httpError{406, "Branch cannot be merged"}
	case "draft_status", "ci_must_pass", "ci_still_running":
		return &httpError{405, "Method Not Allowed"}
	}
	tip := headSHA
	switch {
	case squash:
		tree, err := mergeTree(dir, baseSHA, headSHA)
		if err != nil {
			return &httpError{406, "Branch cannot be merged"}
		}
		if tip, err = git(dir, "commit-tree", tree, "-p", baseSHA, "-m", m.Title+"\n\n"+m.Description); err != nil {
			return err
		}
	case s.MergeMethod != "ff":
		tree, err := mergeTree(dir, baseSHA, headSHA)
		if err != nil {
			return &httpError{406, "Branch cannot be merged"}
		}
		msg := fmt.Sprintf("Merge branch '%s' into '%s'\n\nSee merge request %s!%d", m.Source, m.Target, s.Path, m.IID)
		if tip, err = git(dir, "commit-tree", tree, "-p", baseSHA, "-p", headSHA, "-m", msg); err != nil {
			return err
		}
	}
	if _, err := git(dir, "update-ref", "refs/heads/"+m.Target, tip, baseSHA); err != nil {
		return err
	}
	m.State, m.HeadSHA, m.MergedSHA, m.Squash = "merged", headSHA, tip, squash
	return nil
}

// changedFiles lists the files m changes against its target.
func changedFiles(dir string, m *MR) []string {
	head, base := branchSHA(dir, m.Source), branchSHA(dir, m.Target)
	if m.State != "opened" {
		head = m.HeadSHA
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
