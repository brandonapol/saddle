package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// pushRig is a.Root with a bare origin holding branch b at c1, and a
// second clone to move origin's b behind saddle's back.
func pushRig(t *testing.T) (a *App, origin, other, c1 string) {
	t.Helper()
	a, _ = setup(t)
	origin = t.TempDir()
	git(t, origin, "init", "-q", "--bare", "-b", "main")
	git(t, a.Root, "remote", "add", "origin", origin)
	git(t, a.Root, "push", "-q", "origin", a.Cfg.Base)
	c1 = git(t, a.Root, "rev-parse", a.Cfg.Base)
	git(t, a.Root, "push", "-q", "origin", c1+":refs/heads/b")
	git(t, a.Root, "fetch", "-q", "origin")
	other = filepath.Join(t.TempDir(), "other")
	git(t, filepath.Dir(other), "clone", "-q", origin, other)
	git(t, other, "config", "user.email", "o@x")
	git(t, other, "config", "user.name", "o")
	return a, origin, other, c1
}

func commitIn(t *testing.T, dir, parent, file string) string {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(dir, file), []byte(file), 0o644))
	git(t, dir, "add", file)
	tree := git(t, dir, "write-tree")
	return git(t, dir, "commit-tree", tree, "-p", parent, "-m", file)
}

// The remote branch moved to a commit the push already carries (an earlier
// push saddle didn't fetch): restack re-fetches and pushes instead of giving
// up on "stale info" (#838).
func TestPushLandedRefetchesAfterRemoteMoved(t *testing.T) {
	a, origin, other, c1 := pushRig(t)
	git(t, other, "checkout", "-q", "-b", "b", "origin/b")
	mid := commitIn(t, other, c1, "mid.txt")
	git(t, other, "push", "-q", "origin", mid+":refs/heads/b")
	// saddle's view of origin/b is still c1; its new head builds on mid.
	git(t, a.Root, "fetch", "-q", "origin", "main")
	git(t, a.Root, "fetch", "-q", "origin", "refs/heads/b:refs/heads/mid-local")
	head := commitIn(t, a.Root, mid, "top.txt")
	git(t, a.Root, "update-ref", "refs/remotes/origin/b", c1)

	if err := a.pushLanded("b", head); err != nil {
		t.Fatalf("pushLanded = %v, want it to re-fetch and push", err)
	}
	if got := remoteRev(t, origin, "b"); got != head {
		t.Fatalf("origin/b = %s, want %s", got, head)
	}
}

// The remote branch holds commits the push lacks (a repair pushed): restack
// never overwrites them. It says the branch moved, which auto-merge retries
// later, rather than failing for good.
func TestPushLandedKeepsCommitsItLacks(t *testing.T) {
	a, origin, other, c1 := pushRig(t)
	repair := commitIn(t, other, c1, "repair.txt")
	git(t, other, "push", "-q", "origin", repair+":refs/heads/b")
	head := commitIn(t, a.Root, c1, "top.txt")

	err := a.pushLanded("b", head)
	if !errors.Is(err, ErrRemoteMoved) {
		t.Fatalf("pushLanded = %v, want ErrRemoteMoved", err)
	}
	if got := remoteRev(t, origin, "b"); got != repair {
		t.Fatalf("origin/b = %s, want the repair %s kept", got, repair)
	}
}
