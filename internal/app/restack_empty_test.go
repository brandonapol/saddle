package app

import (
	"os"
	"path/filepath"
	"testing"
)

// The owner pushed straight to main before anything landed: the spawn hook
// says integration is behind and to restack, so restack must move the empty
// integration branch onto origin/main instead of answering "nothing has
// landed yet" (#908 item 2).
func TestRestackMovesEmptyIntegrationOntoBase(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	git(t, a.Root, "branch", a.Cfg.Integration, a.Cfg.Base)
	other := t.TempDir()
	git(t, other, "clone", "-q", origin, ".")
	git(t, other, "config", "user.email", "o@example.com")
	git(t, other, "config", "user.name", "owner")
	must(t, os.WriteFile(filepath.Join(other, ".gitignore"), []byte("build/\n"), 0o644))
	git(t, other, "add", ".gitignore")
	git(t, other, "commit", "-q", "-m", "ignore build")
	git(t, other, "push", "-q", "origin", "HEAD:main")
	want := git(t, other, "rev-parse", "HEAD")

	res, err := a.Restack()
	if err != nil {
		t.Fatalf("restack: %v", err)
	}
	if res.Base != want {
		t.Errorf("Base = %s, want %s", res.Base, want)
	}
	if got := git(t, a.Root, "rev-parse", a.Cfg.Integration); got != want {
		t.Errorf("integration at %s, want origin/main %s", got, want)
	}
	if w := a.integrationBehind("origin/main"); w != "" {
		t.Errorf("still behind: %s", w)
	}
}

// With nothing landed, commits on integration that base lacks are nobody's
// landed work restack can replay, so it refuses and moves nothing.
func TestRestackEmptyKeepsIntegrationThatIsAhead(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	_, _ = originWithGh(t, a)
	git(t, a.Root, "branch", a.Cfg.Integration, a.Cfg.Base)
	wt := t.TempDir()
	git(t, a.Root, "worktree", "add", "-q", wt, a.Cfg.Integration)
	must(t, os.WriteFile(filepath.Join(wt, "x.txt"), []byte("x\n"), 0o644))
	git(t, wt, "add", "x.txt")
	git(t, wt, "commit", "-q", "-m", "stray")
	git(t, a.Root, "worktree", "remove", "--force", wt)
	old := git(t, a.Root, "rev-parse", a.Cfg.Integration)
	if _, err := a.Restack(); err == nil {
		t.Fatal("restack moved an integration branch with stray commits")
	}
	if got := git(t, a.Root, "rev-parse", a.Cfg.Integration); got != old {
		t.Errorf("integration moved to %s", got)
	}
}
