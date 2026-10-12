package app

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// refuseAsQueued makes origin refuse pushes to branch the way GitHub refuses
// a push to a PR head that is in the merge queue.
func refuseAsQueued(t *testing.T, origin, branch string) {
	t.Helper()
	hook := "#!/bin/sh\nwhile read old new ref; do\n" +
		"  if [ \"$ref\" = \"refs/heads/" + branch + "\" ]; then\n" +
		"    echo 'error: GH006: Protected branch update failed for refs/heads/" + branch + ".' >&2\n" +
		"    echo 'error: Changes must be made through the merge queue' >&2\n" +
		"    exit 1\n  fi\ndone\n"
	must(t, os.WriteFile(filepath.Join(origin, "hooks", "pre-receive"), []byte(hook), 0o755))
}

// #431: a layer whose PR sits in GitHub's merge queue refuses the push with
// GH006. Restack defers that layer and the ones above it, says so, and still
// publishes the layers below.
func TestRestackDefersLayerInMergeQueue(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	origin, ghLog := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	landTask(t, a, "t3", "three", map[string]string{"three.txt": "three\n"})
	_, err := a.PRs()
	must(t, err)
	t1, _ := a.Store.Task("t1")
	t2, _ := a.Store.Task("t2")
	t3, _ := a.Store.Task("t3")
	moveMain(t, origin, "news.txt")
	refuseAsQueued(t, origin, t2.Branch)
	before := len(ghLog())

	res, err := a.Restack()
	if err != nil {
		t.Fatalf("Restack: %v", err)
	}
	if !slices.Equal(res.Queued, []string{"t2", "t3"}) {
		t.Fatalf("queued = %v, want [t2 t3]", res.Queued)
	}
	if !slices.Equal(res.Retargeted, []string{"t1"}) {
		t.Fatalf("retargeted = %v, want [t1]", res.Retargeted)
	}
	if got := git(t, origin, "rev-parse", "refs/heads/"+t1.Branch); got != git(t, a.Root, "rev-parse", t1.Branch) {
		t.Fatalf("t1 not pushed: origin %s", got)
	}
	if got := git(t, origin, "rev-parse", "refs/heads/"+t3.Branch); got == git(t, a.Root, "rev-parse", t3.Branch) {
		t.Fatalf("t3 was pushed above a queued layer")
	}
	for _, tk := range []string{t2.PR, t3.PR} {
		for _, c := range prCalls(ghLog()[before:], tk) {
			if strings.HasPrefix(c, "pr edit") {
				t.Fatalf("restack edited a deferred PR: %s", c)
			}
		}
	}
	if !hasEvent(t, a, EventRestackQueued, "GH006") {
		t.Fatalf("no %s event for t2", EventRestackQueued)
	}
	if !hasEvent(t, a, EventNoticeDigest, "merge queue") {
		t.Fatalf("the orchestrator was not told about the merge queue")
	}

	// Once the queue lets go, the next restack publishes the deferred layers.
	must(t, os.Remove(filepath.Join(origin, "hooks", "pre-receive")))
	res, err = a.Restack()
	must(t, err)
	if len(res.Queued) != 0 {
		t.Fatalf("queued again = %v", res.Queued)
	}
	if got := git(t, origin, "rev-parse", "refs/heads/"+t3.Branch); got != git(t, a.Root, "rev-parse", t3.Branch) {
		t.Fatalf("t3 not pushed on the next restack")
	}
}
