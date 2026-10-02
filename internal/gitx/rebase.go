package gitx

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// RebaseInProgress reports whether dir's worktree is stopped mid-rebase.
func RebaseInProgress(dir string) bool {
	for _, p := range []string{"rebase-merge", "rebase-apply"} {
		path, err := Run(dir, "rev-parse", "--path-format=absolute", "--git-path", p)
		if err != nil {
			continue
		}
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// PatchIDs returns the stable patch-id of each non-merge commit in rng,
// oldest first.
func PatchIDs(dir, rng string) ([]string, error) {
	log, err := Run(dir, "log", "-p", "--reverse", "--no-merges", "--no-color", "--no-ext-diff", rng)
	if err != nil {
		return nil, err
	}
	out, err := PatchID(dir, log)
	if err != nil || out == "" {
		return nil, err
	}
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		id, _, _ := strings.Cut(line, " ")
		ids = append(ids, id)
	}
	return ids, nil
}

// PatchID runs `git patch-id --stable` over a diff or log.
func PatchID(dir, diff string) (string, error) {
	calls.Add(1)
	cmd := exec.Command("git", "-C", dir, "patch-id", "--stable")
	cmd.Stdin = strings.NewReader(diff + "\n")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git patch-id: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// LandedPrefix finds how much of HEAD's own history (the first-parent
// commits since its merge-base with onto) onto already holds under other
// SHAs, as when the commits HEAD was cut from were later squash-landed. It
// returns the newest such commit and how many own commits it covers, or ""
// when none are. A commit is held when its tree is one onto has had, or when
// the combined diff since the previous held commit has the patch-id of a
// commit on onto. Rebasing with --onto onto from there replays only the rest.
func LandedPrefix(dir, onto string) (string, int, error) {
	mb, err := Run(dir, "merge-base", "HEAD", onto)
	if err != nil {
		return "", 0, err
	}
	own, err := Run(dir, "log", "--reverse", "--first-parent", "--format=%H %T", mb+"..HEAD")
	if err != nil || own == "" {
		return "", 0, err
	}
	theirs, err := Run(dir, "log", "--format=%T", mb+".."+onto)
	if err != nil || theirs == "" {
		return "", 0, err
	}
	trees := map[string]bool{}
	for _, t := range strings.Split(theirs, "\n") {
		trees[t] = true
	}
	ids, err := PatchIDs(dir, mb+".."+onto)
	if err != nil {
		return "", 0, err
	}
	landed := map[string]bool{}
	for _, id := range ids {
		landed[id] = true
	}
	from, held, n := mb, "", 0
	for i, line := range strings.Split(own, "\n") {
		c, tree, _ := strings.Cut(line, " ")
		ok := trees[tree]
		if !ok {
			diff, err := Run(dir, "diff", "--no-color", "--no-ext-diff", from, c)
			if err != nil {
				return "", 0, err
			}
			if diff != "" {
				out, err := PatchID(dir, diff)
				if err != nil {
					return "", 0, err
				}
				id, _, _ := strings.Cut(out, " ")
				ok = landed[id]
			}
		}
		if ok {
			from, held, n = c, c, i+1
		}
	}
	return held, n, nil
}
