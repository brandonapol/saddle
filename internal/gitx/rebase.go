package gitx

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
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

// ForkPoint is the newest commit in HEAD's history that ref has ever pointed
// at, read from ref's reflog, or "" when the reflog doesn't know one (a fresh
// clone, an expired or deleted reflog).
func ForkPoint(dir, ref string) string {
	fp, err := Run(dir, "merge-base", "--fork-point", ref, "HEAD")
	if err != nil {
		return ""
	}
	return fp
}

// ReplayFrom is where a rebase of HEAD onto onto should start replaying, for
// `rebase --onto onto <from>`, and how many first-parent commits since the
// merge-base it leaves out; "" means a plain rebase. It skips the commits
// LandedPrefix finds onto already holds and, when track is set, every commit
// up to track's fork point: commits track once held (HEAD was cut from or
// synced onto them) that it later rewrote, as restack does after a squash
// merge. Commits track still holds are left to the merge-base as before.
func ReplayFrom(dir, onto, track string) (string, int, error) {
	from, n, err := LandedPrefix(dir, onto)
	if err != nil || track == "" {
		return from, n, err
	}
	fp := ForkPoint(dir, track)
	if fp == "" || fp == from || isAncestor(dir, fp, onto) {
		return from, n, nil
	}
	if from != "" && !isAncestor(dir, from, fp) {
		return from, n, nil
	}
	mb, err := Run(dir, "merge-base", "HEAD", onto)
	if err != nil {
		return "", 0, err
	}
	c, err := Run(dir, "rev-list", "--count", "--first-parent", mb+".."+fp)
	if err != nil {
		return "", 0, err
	}
	if n, err = strconv.Atoi(c); err != nil {
		return "", 0, err
	}
	return fp, n, nil
}

func isAncestor(dir, a, b string) bool {
	_, err := Run(dir, "merge-base", "--is-ancestor", a, b)
	return err == nil
}
