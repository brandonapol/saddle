package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #433: a layer whose PR sits in GitHub's merge queue refuses the push with
// GH006. prs defers that layer and the ones stacked on it, says so, and still
// publishes the others.
func TestPRsDefersLayerInMergeQueue(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	landTask(t, a, "t3", "three", map[string]string{"three.txt": "three\n"})
	landTask(t, a, "t4", "two more", map[string]string{"two.txt": "two\nmore\n"})
	refuseAsQueued(t, origin, t2.Branch)

	urls, err := a.PRs()
	if err != nil {
		t.Fatalf("PRs: %v", err)
	}
	if len(urls) != 2 {
		t.Fatalf("urls = %v, want t1's and t3's PRs", urls)
	}
	for id, want := range map[string]bool{"t1": true, "t2": false, "t3": true, "t4": false} {
		tk, _ := a.Store.Task(id)
		if (tk.PR != "") != want {
			t.Fatalf("%s PR = %q, want opened %v", id, tk.PR, want)
		}
	}
	for _, c := range ghLog() {
		if strings.HasPrefix(c, "pr create") && strings.Contains(c, t2.Branch) {
			t.Fatalf("prs opened a PR on the queued branch: %s", c)
		}
	}
	if !hasEvent(t, a, EventPRsQueued, "GH006") {
		t.Fatalf("no %s event for t2", EventPRsQueued)
	}
	if !hasEvent(t, a, EventNoticeDigest, "merge queue") {
		t.Fatalf("the orchestrator was not told about the merge queue")
	}

	// Once the queue lets go, the next prs publishes the deferred layers.
	must(t, os.Remove(filepath.Join(origin, "hooks", "pre-receive")))
	_, err = a.PRs()
	must(t, err)
	for _, id := range []string{"t2", "t4"} {
		if tk, _ := a.Store.Task(id); tk.PR == "" {
			t.Fatalf("%s not published on the next prs", id)
		}
	}
}
