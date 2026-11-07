package gc

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// BackupPrefix holds the integration branch's backups, one per rewrite,
// named <UTC time>-<short sha> so they sort oldest first.
const BackupPrefix = "refs/saddle/integration-backups/"

// BackupKeep is how many integration backups automatic gc keeps.
const BackupKeep = 20

func git(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// PruneBackups deletes all but the newest keep refs under prefix, by name,
// and returns the ones it deleted. A ref that won't delete is reported in
// the error and the rest still go.
func PruneBackups(dir, prefix string, keep int) ([]string, error) {
	out, err := git(dir, "for-each-ref", "--sort=refname", "--format=%(refname)", prefix)
	if err != nil || out == "" {
		return nil, err
	}
	refs := strings.Split(out, "\n")
	if len(refs) <= keep {
		return nil, nil
	}
	var gone []string
	var errs []error
	for _, r := range refs[:len(refs)-max(keep, 0)] {
		if _, err := git(dir, "update-ref", "-d", r); err != nil {
			errs = append(errs, err)
			continue
		}
		gone = append(gone, r)
	}
	return gone, errors.Join(errs...)
}

// Maintain runs git's own upkeep on the repo at dir: loose objects are
// packed (the loose-objects task ignores gc.auto=0, which quark has), packed
// loose copies dropped, and gc runs when gc.auto says it is due. Nothing
// reachable or recently written is pruned: that is git gc's own policy.
func Maintain(dir string) error {
	return errors.Join(
		run1(dir, "maintenance", "run", "--auto", "--task=loose-objects"),
		run1(dir, "prune-packed"),
		run1(dir, "maintenance", "run", "--auto", "--task=gc"),
	)
}

func run1(dir string, args ...string) error {
	_, err := git(dir, args...)
	return err
}
