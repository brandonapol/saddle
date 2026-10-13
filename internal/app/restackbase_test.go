package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// linkPRsOnGitHub makes the fake gh refuse any base edit, as GitHub does for
// a PR that gh stack linked: "Cannot change the base branch because the pull
// request is part of a stack" (#341).
func linkPRsOnGitHub(t *testing.T, a *App) {
	t.Helper()
	bin := filepath.Join(a.Root, ".git", FakeGHSubdir)
	must(t, os.Rename(filepath.Join(bin, "gh"), filepath.Join(bin, "gh.unlinked")))
	script := `#!/bin/sh
if [ "$1 $2" = "pr edit" ] && [ "$4" = "--base" ]; then
	echo "$*" >> "` + filepath.Join(bin, "gh.log") + `"
	echo "GraphQL: Cannot change the base branch because the pull request is part of a stack. (updatePullRequest)" >&2
	exit 1
fi
exec "` + filepath.Join(bin, "gh.unlinked") + `" "$@"
`
	must(t, os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755))
}

// #341: a stack linked with gh stack whose lower layer was amended (base
// moved under it, so restack re-cuts it). Every PR already targets the right
// branch, so restack edits no base, and the layer above is re-cut onto the
// amended one and pushed rather than left holding the old copy.
func TestRestackLinkedStackLowerLayerAmended(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Train.StackBackend = "gh-stack"
	origin, ghLog := originWithGh(t, a)
	f := useFakeStackGH(t, a, nil)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"one.txt": "one\ntwo\n"}) // stacks on t1
	_, err := a.PRs()
	must(t, err)
	t1, t2 := mustTask(t, a, "t1"), mustTask(t, a, "t2")
	setPR(t, t1.PR, "OPEN", "main")
	setPR(t, t2.PR, "OPEN", t1.Branch)
	linkPRsOnGitHub(t, a)
	old1 := remoteRev(t, origin, t1.Branch)
	moveMain(t, origin, "news.txt")
	links, before := len(f.links()), len(ghLog())

	if _, err := a.Restack(); err != nil {
		t.Fatalf("Restack: %v", err)
	}
	for _, c := range ghLog()[before:] {
		if strings.HasPrefix(c, "pr edit") {
			t.Fatalf("restack edited a base that was already right: %s", c)
		}
	}
	new1, new2 := remoteRev(t, origin, t1.Branch), remoteRev(t, origin, t2.Branch)
	if new1 == old1 {
		t.Fatalf("t1 wasn't re-cut onto the moved base")
	}
	if parent := git(t, origin, "rev-parse", new2+"^"); parent != new1 {
		t.Fatalf("t2's pushed head sits on %s, want t1's amended head %s", short(parent), short(new1))
	}
	if len(f.links()) != links+1 {
		t.Fatalf("restack didn't relink the stack: %q", f.links()[links:])
	}
}

// #341: when a linked PR's base does need to change, GitHub refuses the edit;
// restack carries on, pushes every layer and relinks the stack, which sets
// the bases, instead of failing halfway and leaving layers diverged.
func TestRestackLinkedStackRefusedBaseEdit(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Train.StackBackend = "gh-stack"
	origin, _ := originWithGh(t, a)
	f := useFakeStackGH(t, a, nil)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"one.txt": "one\ntwo\n"})
	_, err := a.PRs()
	must(t, err)
	t1, t2 := mustTask(t, a, "t1"), mustTask(t, a, "t2")
	setPR(t, t1.PR, "OPEN", "main")
	setPR(t, t2.PR, "OPEN", "main") // wrong: the layout puts it on t1
	linkPRsOnGitHub(t, a)
	moveMain(t, origin, "news.txt")
	links := len(f.links())

	if _, err := a.Restack(); err != nil {
		t.Fatalf("Restack: %v", err)
	}
	new1, new2 := remoteRev(t, origin, t1.Branch), remoteRev(t, origin, t2.Branch)
	if parent := git(t, origin, "rev-parse", new2+"^"); parent != new1 {
		t.Fatalf("t2's pushed head sits on %s, want t1's head %s", short(parent), short(new1))
	}
	if len(f.links()) != links+1 {
		t.Fatalf("restack didn't relink the stack: %q", f.links()[links:])
	}
	if evs := eventsOf(t, a, EventPRBaseLinked); len(evs) != 1 || !strings.Contains(evs[0], t2.PR) {
		t.Fatalf("%s events = %q, want one for %s", EventPRBaseLinked, evs, t2.PR)
	}
}
