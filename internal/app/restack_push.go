package app

import (
	"errors"
	"fmt"

	"github.com/brandonapol/saddle/internal/gitx"
)

// ErrRemoteMoved is a leased push refused because the remote branch holds
// commits the pushed head lacks: someone (a repair worker) pushed there.
// Restack never overwrites them. It is a wait, not a failure: once that
// work lands in the train, the next restack carries it (#838).
var ErrRemoteMoved = errors.New("remote branch moved to commits the restack doesn't have")

// pushAfterMove handles a leased push of sha to branch that failed with
// pushErr while the branch exists on origin. It re-fetches the branch. When
// what is there now is already in sha (an earlier push saddle didn't fetch,
// or a stale remote-tracking ref), it pushes again, leased on the fresh
// tip; otherwise it returns ErrRemoteMoved and leaves the branch alone.
func (a *App) pushAfterMove(branch, sha string, pushErr error) error {
	ref := "refs/remotes/origin/" + branch
	if _, err := trainGit(a.Root, "fetch", "--no-tags", "origin", "+refs/heads/"+branch+":"+ref); err != nil {
		return fmt.Errorf("%w (re-fetch of %s failed: %v)", pushErr, branch, err)
	}
	tip, err := gitx.RevParse(a.Root, ref)
	if err != nil || tip == "" {
		return pushErr
	}
	if tip == sha {
		return nil
	}
	if _, err := gitx.Run(a.Root, "merge-base", "--is-ancestor", tip, sha); err != nil {
		return fmt.Errorf("push %s to %s: %w (origin has %s): %v", short(sha), branch, ErrRemoteMoved, short(tip), pushErr)
	}
	_, err = trainGit(a.Root, "push", "--force-with-lease=refs/heads/"+branch+":"+tip, "origin", sha+":refs/heads/"+branch)
	return err
}
