package app

import (
	"os"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// #765: a task whose worktree vanished but whose branch has work gets the
// worktree back at its branch and is relaunched with its brief, once.
func TestResumeRecreatesMissingWorktreeFromBranch(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	a, ft := setup(t)
	tk, err := a.Spawn(SpawnReq{Title: "one", Prompt: "do one", Claims: []string{"one/**"}})
	must(t, err)
	write(t, tk.Worktree, "one/a.txt", "work\n")
	commitAll(t, tk.Worktree, "a")
	withTranscript(t, a, tk.ID, "sess-1")
	serverDies(ft)
	must(t, os.RemoveAll(tk.Worktree))

	rs, err := a.ResumeLost(false)
	must(t, err)
	if len(rs) != 1 || rs[0].Task != tk.ID {
		t.Fatalf("resumed %+v, want %s", rs, tk.ID)
	}
	if b, err := os.ReadFile(tk.Worktree + "/one/a.txt"); err != nil || string(b) != "work\n" {
		t.Fatalf("worktree not recreated at its branch: %q, %v", b, err)
	}
	if br := git(t, tk.Worktree, "rev-parse", "--abbrev-ref", "HEAD"); br != tk.Branch {
		t.Fatalf("recreated worktree is on %q, want %s", br, tk.Branch)
	}
	got, _ := a.Store.Task(tk.ID)
	if !a.ownWindow(got) || got.Status != store.Running {
		t.Fatalf("after resume: window %q status %s", got.Window, got.Status)
	}
	if s := launchScript(t, a, tk.ID); strings.Contains(s, "--resume") {
		t.Errorf("a recreated worktree relaunches with its brief, not the old session:\n%s", s)
	}
	if p := runFile(t, a, tk.ID, "prompt.md"); !strings.Contains(p, "do one") || !strings.Contains(p, "may already exist") {
		t.Errorf("relaunch prompt = %q", p)
	}
	if n := countEvents(t, a, eventWorktreeRecreated, ""); n != 1 {
		t.Errorf("worktree_recreated events = %d, want 1", n)
	}
	if n := countEvents(t, a, EventNoticeDigest, "recreated"); n != 1 {
		t.Errorf("orchestrator notices about the recreated worktree = %d, want 1", n)
	}
	if rs, err := a.ResumeLost(false); err != nil || len(rs) != 0 {
		t.Fatalf("second pass resumed %+v, %v", rs, err)
	}
}

// #765: a task whose worktree and branch are both gone is retired: killed
// (by saddle, not the owner), its claims released, and never retried.
func TestResumeRetiresTaskWithNoWorktreeOrBranch(t *testing.T) {
	t.Setenv("SADDLE_REFGUARD", "off") // the test deletes a live task's branch
	a, ft := setup(t)
	tk, err := a.Spawn(SpawnReq{Title: "one", Claims: []string{"one/**"}})
	must(t, err)
	serverDies(ft)
	must(t, os.RemoveAll(tk.Worktree))
	git(t, a.Root, "worktree", "prune")
	git(t, a.Root, "branch", "-D", tk.Branch)

	rs, err := a.ResumeLost(false)
	if err != nil || len(rs) != 0 {
		t.Fatalf("first pass: resumed %+v, err %v; want the task retired quietly", rs, err)
	}
	assertRetired(t, a, tk.ID)
	if rs, err := a.ResumeLost(false); err != nil || len(rs) != 0 {
		t.Fatalf("second pass: resumed %+v, err %v", rs, err)
	}
	if n := countEvents(t, a, eventWorktreeLost, ""); n != 1 {
		t.Errorf("worktree_lost events = %d, want 1", n)
	}
}

// #765: a branch with no commits beyond integration has nothing to save, so
// the task is retired rather than relaunched from scratch.
func TestResumeRetiresTaskWhoseBranchHasNoWork(t *testing.T) {
	t.Parallel()
	a, ft := setup(t)
	tk, err := a.Spawn(SpawnReq{Title: "one", Claims: []string{"one/**"}})
	must(t, err)
	serverDies(ft)
	must(t, os.RemoveAll(tk.Worktree))

	if rs, err := a.ResumeLost(false); err != nil || len(rs) != 0 {
		t.Fatalf("resumed %+v, err %v; want the task retired", rs, err)
	}
	assertRetired(t, a, tk.ID)
}

// saddle resume on such a task (autopilot Reconcile's path) retires it too,
// and says so.
func TestResumeOneRetiresTaskWithNoWorktree(t *testing.T) {
	t.Parallel()
	a, ft := setup(t)
	tk, err := a.Spawn(SpawnReq{Title: "one"})
	must(t, err)
	serverDies(ft)
	must(t, os.RemoveAll(tk.Worktree))
	if _, err := a.Resume(tk.ID); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("Resume = %v, want a retired error", err)
	}
	assertRetired(t, a, tk.ID)
}

func assertRetired(t *testing.T, a *App, id string) {
	t.Helper()
	got, _ := a.Store.Task(id)
	if got.Status != store.Killed {
		t.Fatalf("%s is %s, want killed", id, got.Status)
	}
	if by := a.killedBy(id); by != KilledByWorktreeLost {
		t.Errorf("killed by %q, want %q", by, KilledByWorktreeLost)
	}
	if cl, _ := a.Store.Claims(); len(cl[id]) != 0 {
		t.Errorf("claims kept: %v", cl[id])
	}
	if n := countEvents(t, a, EventNoticeDigest, "retired "+id); n != 1 {
		t.Errorf("orchestrator notices about the retired task = %d, want 1", n)
	}
}
