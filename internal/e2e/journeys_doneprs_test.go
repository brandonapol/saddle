//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/e2e/fakegh"
	"github.com/brandonapol/saddle/internal/mcpserver"
	"github.com/brandonapol/saddle/internal/prgraph"
)

// TestJourneyDonePublishesOwnPR (ADR 0012, #932, #933): with done's default
// path, two agents on disjoint work each get one PR against main holding only
// their own commit, opened by done itself: no train, no land, no prs.
func TestJourneyDonePublishesOwnPR(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{Publish: true})
	open := func(v mcpserver.TaskView) bool { return v.Status == "pr_open" && v.PR != "" }
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, finished("alpha", "alpha\n")...)
	w.Spawn("t2", "Beta work", []string{"beta/**"}, finished("beta", "beta\n")...)
	urls := map[string]string{}
	for _, id := range []string{"t1", "t2"} {
		urls[id] = w.WaitTask(id, "its PR open", open).PR
	}
	if urls["t1"] == urls["t2"] {
		t.Fatalf("both tasks share PR %s", urls["t1"])
	}
	s, err := w.GH.Load()
	if err != nil {
		t.Fatal(err)
	}
	for id, dir := range map[string]string{"t1": "alpha", "t2": "beta"} {
		p := prNumber(t, s, urls[id])
		if p.Base != "main" {
			t.Errorf("%s's PR targets %s, want main", id, p.Base)
		}
		files := w.Git(w.Origin, "diff", "--name-only", "main..."+p.Head)
		if strings.TrimSpace(files) != dir+"/work.txt" {
			t.Errorf("%s's PR changes %q, want only %s/work.txt", id, files, dir)
		}
		if v := w.Task(id); v.Train != "" {
			t.Errorf("%s is in the train: %q", id, v.Train)
		}
	}
	if n := len(s.PRs); n != 2 {
		t.Fatalf("fake GitHub has %d PRs, want 2", n)
	}
}

// TestJourneyQueueMergesPublishedPR (ADR 0012, #935): with legacy_train off,
// an agent's done opens its PR against main, and the merge queue, not the
// old auto-merge watcher, merges it once CI is green: main gets the work and
// the task ends merged.
func TestJourneyQueueMergesPublishedPR(t *testing.T) {
	if Fork(t) {
		return
	}
	w := world(t, Options{Publish: true, Tables: "[train]\nauto_merge = true\n"})
	must(t, w.GH.Update(func(s *fakegh.State) error {
		s.DefaultChecks = []fakegh.Check{{Name: "ci", State: fakegh.Pending}}
		return nil
	}))
	w.Spawn("t1", "Alpha work", []string{"alpha/**"}, finished("alpha", "alpha\n")...)
	url := w.WaitTask("t1", "its PR open", func(v mcpserver.TaskView) bool { return v.Status == "pr_open" && v.PR != "" }).PR

	// CI pending: the queue waits.
	if tk := w.QueueTick(); tk.Applied != nil {
		t.Fatalf("the queue applied %v while CI is pending", tk.Applied)
	}
	must(t, w.GH.Update(func(s *fakegh.State) error {
		for _, p := range s.PRs {
			for i := range p.Checks {
				p.Checks[i].State = fakegh.Pass
			}
		}
		return nil
	}))
	tk := w.QueueTick()
	if tk.Applied == nil || tk.Applied.Kind != prgraph.ActMerge || tk.Result != "ok" {
		t.Fatalf("the queue applied %v (%s), want the merge", tk.Applied, tk.Result)
	}
	s, err := w.GH.Load()
	if err != nil {
		t.Fatal(err)
	}
	if p := prNumber(t, s, url); p.State != "MERGED" || p.MergedBy != "squash" {
		t.Fatalf("PR %s is %s by %q, want MERGED by squash", url, p.State, p.MergedBy)
	}
	if out := w.Git(w.Origin, "show", "main:alpha/work.txt"); strings.TrimSpace(out) != "alpha" {
		t.Fatalf("main's alpha/work.txt = %q", out)
	}
	if v := w.Task("t1"); v.Status != "merged" {
		t.Fatalf("t1 is %s, want merged", v.Status)
	}
	if tk := w.QueueTick(); tk.Applied != nil {
		t.Fatalf("a second tick applied %v", tk.Applied)
	}
}
