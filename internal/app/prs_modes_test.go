package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPRsPreviewChangesNoRefsStateWorktreesOrRemote(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, calls := originWithGh(t, a)
	first := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	second := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	marker := filepath.Join(t.TempDir(), "ran")
	a.Cfg.Train.Prepublish.Cmd = "echo gate > " + shellQuote(marker)
	must(t, os.WriteFile(filepath.Join(a.Root, ".git", "hooks", "post-commit"), []byte("#!/bin/sh\necho hook > "+shellQuote(marker)+"\n"), 0o755))
	refs := git(t, a.Root, "for-each-ref", "--format=%(refname) %(objectname)")
	worktrees := git(t, a.Root, "worktree", "list", "--porcelain")
	events, err := a.Store.Events(1000)
	must(t, err)
	plan, err := a.PreviewPRs()
	must(t, err)
	if len(plan.Layers) != 2 || !plan.Layers[1].Recut || plan.Layers[1].Base != "main" || len(plan.Checks) != 1 {
		t.Fatalf("preview=%+v", plan)
	}
	if got := git(t, a.Root, "for-each-ref", "--format=%(refname) %(objectname)"); got != refs {
		t.Fatal("preview moved refs")
	}
	if got := git(t, a.Root, "worktree", "list", "--porcelain"); got != worktrees {
		t.Fatal("preview registered worktrees")
	}
	after, err := a.Store.Events(1000)
	must(t, err)
	if len(after) != len(events) {
		t.Fatal("preview wrote events")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("preview ran gate or hook: %v", err)
	}
	if len(calls()) != 0 || remoteRev(t, origin, first.Branch) != "" || remoteRev(t, origin, second.Branch) != "" {
		t.Fatal("preview contacted GitHub or pushed")
	}
	stack, err := a.landedStack()
	must(t, err)
	layout, _, err := a.stackLayout(stack)
	must(t, err)
	for i, layer := range plan.Layers {
		if layer.Head != layout[i].Head {
			t.Fatalf("preview head %s differs from publish head %s", layer.Head, layout[i].Head)
		}
	}
}

func TestPRsPushOnlyDoesNotCreateOrEditPRs(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, calls := originWithGh(t, a)
	tasks := landUnrelated(t, a, 2)
	branches, err := a.PRsWithOptions(PRsOptions{PushOnly: true})
	must(t, err)
	if len(branches) != 2 {
		t.Fatalf("pushed branches=%v", branches)
	}
	for _, task := range tasks {
		if remoteRev(t, origin, task.Branch) == "" {
			t.Fatalf("%s not pushed", task.Branch)
		}
		if got := mustTask(t, a, task.ID); got.PR != "" {
			t.Fatalf("PR created: %+v", got)
		}
	}
	for _, call := range calls() {
		if strings.HasPrefix(call, "pr create") || strings.HasPrefix(call, "pr edit") || strings.HasPrefix(call, "pr comment") {
			t.Fatalf("push-only wrote PR: %s", call)
		}
	}
}

func TestPRsGateOnlyRunsChecksWithoutMovingOrPushingBranches(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, calls := originWithGh(t, a)
	tasks := landUnrelated(t, a, 2)
	marker := filepath.Join(t.TempDir(), "checks")
	a.Cfg.Train.Prepublish.Cmd = "pwd >> " + shellQuote(marker)
	refs := git(t, a.Root, "for-each-ref", "--format=%(refname) %(objectname)")
	_, err := a.PRsWithOptions(PRsOptions{GateOnly: true})
	must(t, err)
	b, err := os.ReadFile(marker)
	must(t, err)
	if strings.Count(string(b), "prgate") != 2 {
		t.Fatalf("gate output=%s", b)
	}
	if got := git(t, a.Root, "for-each-ref", "--format=%(refname) %(objectname)"); got != refs {
		t.Fatal("gate-only moved branches")
	}
	if len(calls()) != 0 {
		t.Fatalf("gate-only contacted GitHub: %v", calls())
	}
	for _, task := range tasks {
		if remoteRev(t, origin, task.Branch) != "" {
			t.Fatal("gate-only pushed")
		}
	}
}
