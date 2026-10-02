package sentinel

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/store"
)

// #119.1: merged and closed PRs and killed tasks leave the stack for good.
// None of them is labeled, looked up again, or keeps the stack flagged, and
// once restacked the stack checks clean with no hand edit.
func TestSentinelDropsMergedClosedAndKilled(t *testing.T) {
	a, origin := setup(t)
	gh := newFakeGH(t)
	var ts []store.Task
	for _, id := range []string{"t1", "t2", "t3", "t4"} {
		ts = append(ts, landTask(t, a, id, "f"+id))
	}
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	for i := range ts {
		ts[i], _ = a.Store.Task(ts[i].ID)
	}
	t1, t2, t3, t4 := ts[0], ts[1], ts[2], ts[3]
	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	git(t, other, "merge", "-q", "--squash", "origin/"+t1.Branch)
	git(t, other, "commit", "-qm", "ft1 (#1)")
	git(t, other, "push", "-q", "origin", "main")
	gh.setPR(t1.PR, "MERGED", "UNKNOWN")
	gh.setPR(t2.PR, "CLOSED", "UNKNOWN")
	must(t, a.Kill(t3.ID, false))

	s := New(a)
	rep, err := s.Check()
	must(t, err)
	if !rep.AtRisk || rep.Task != t4.ID {
		t.Fatalf("report = %+v, want only t4 needing a restack", rep)
	}
	for id, want := range map[string]string{t1.ID: app.TrainMerged, t2.ID: app.TrainSuperseded, t3.ID: app.TrainSuperseded} {
		if got := trainState(t, a, id); got != want {
			t.Fatalf("%s's train row = %q, want %q", id, got, want)
		}
	}
	before := len(gh.log())
	if _, err := s.Check(); err != nil {
		t.Fatal(err)
	}
	for _, c := range gh.log()[before:] {
		for _, tk := range []store.Task{t1, t2, t3} {
			if strings.Contains(c, tk.PR+" ") || strings.HasSuffix(c, tk.PR) {
				t.Fatalf("%s left the stack but its PR was touched: %s", tk.ID, c)
			}
		}
	}

	if _, err := a.Restack(); err != nil {
		t.Fatalf("Restack: %v", err)
	}
	if rep, err := s.Check(); err != nil || rep.AtRisk {
		t.Fatalf("after restack: %+v, %v", rep, err)
	}
	if _, flagged, _ := a.Flag(); flagged {
		t.Fatal("stack still flagged")
	}
	for _, c := range gh.log() {
		if strings.Contains(c, "--add-label") {
			t.Fatalf("labeled without a conflict: %s", c)
		}
	}
	if _, err := a.PRs(); err != nil {
		t.Fatalf("PRs: %v", err)
	}
}

// #119.5: sentinel ack lifts the freeze and labels of the current flag
// without editing state.db; it stays quiet while the same layer is the
// problem, and a clean check forgets the ack.
func TestSentinelAck(t *testing.T) {
	a, _ := setup(t)
	gh := newFakeGH(t)
	t1 := landTask(t, a, "t1", "one")
	landTask(t, a, "t2", "two")
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	t1, _ = a.Store.Task(t1.ID)
	gh.setPR(t1.PR, "OPEN", "CONFLICTING")
	s := New(a)
	if rep, err := s.Check(); err != nil || !rep.AtRisk || len(rep.PRs) != 2 {
		t.Fatalf("check = %+v, %v", rep, err)
	}

	if _, err := a.AckFlag(); err != nil {
		t.Fatal(err)
	}
	before := len(gh.log())
	rep, err := s.Check()
	must(t, err)
	if !rep.Acked {
		t.Fatalf("report = %+v, want acked", rep)
	}
	if !hasCall(gh.log()[before:], "pr edit "+t1.PR+" --remove-label "+Label) {
		t.Fatalf("ack left the label: %v", gh.log()[before:])
	}
	f, flagged, _ := a.Flag()
	if !flagged || !f.Acked || len(f.PRs) != 0 {
		t.Fatalf("flag after ack = %+v %v", f, flagged)
	}
	if _, err := a.PRs(); err != nil {
		t.Fatalf("PRs after ack: %v", err)
	}
	if n := len(events(t, a, EventAtRisk)); n != 1 {
		t.Fatalf("acked flag re-raised: %d events", n)
	}

	gh.setPR(t1.PR, "OPEN", "MERGEABLE")
	if rep, err := s.Check(); err != nil || rep.AtRisk {
		t.Fatalf("clean check = %+v, %v", rep, err)
	}
	if _, flagged, _ := a.Flag(); flagged {
		t.Fatal("ack outlived a clean check")
	}
	gh.setPR(t1.PR, "OPEN", "CONFLICTING")
	if rep, err := s.Check(); err != nil || rep.Acked || !rep.AtRisk {
		t.Fatalf("new conflict after the ack = %+v, %v", rep, err)
	}
}

// Labels a conflict put on come off once only a routine restack is left.
func TestSentinelDropsLabelsWhenConflictGoes(t *testing.T) {
	a, origin := setup(t)
	gh := newFakeGH(t)
	t1 := landTask(t, a, "t1", "one")
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	t1, _ = a.Store.Task(t1.ID)
	gh.setPR(t1.PR, "OPEN", "CONFLICTING")
	s := New(a)
	if rep, err := s.Check(); err != nil || len(rep.PRs) != 1 {
		t.Fatalf("check = %+v, %v", rep, err)
	}
	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	write(t, other, "news.txt", "news\n")
	commitAll(t, other, "news")
	git(t, other, "push", "-q", "origin", "main")
	gh.setPR(t1.PR, "OPEN", "MERGEABLE")
	before := len(gh.log())
	rep, err := s.Check()
	must(t, err)
	if !rep.AtRisk || len(rep.PRs) != 0 {
		t.Fatalf("report = %+v, want at risk with no labels", rep)
	}
	if !hasCall(gh.log()[before:], "pr edit "+t1.PR+" --remove-label "+Label) {
		t.Fatalf("label kept: %v", gh.log()[before:])
	}
}
