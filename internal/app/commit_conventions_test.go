package app

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

func TestWorkerBriefKeepsRepositoryCommitConventions(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	brief := a.workerBrief(store.Task{ID: "t1", Title: "fix: correct capacity"}, nil)
	for _, want := range []string{"repository's commit and PR title conventions", "Never add a saddle: subject prefix"} {
		if !strings.Contains(brief, want) {
			t.Errorf("brief missing %q", want)
		}
	}
}

func TestLandAndPRsPreserveConventionalTitles(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	_, calls := originWithGh(t, a)
	subject := "fix(storage): report capacity correctly"
	task := landTask(t, a, "t1", subject, map[string]string{"capacity.txt": "fixed\n"})
	if got := git(t, a.Root, "log", "-1", "--format=%s", task.Branch); got != subject {
		t.Fatalf("landed subject = %q", got)
	}
	_, err := a.PRs()
	must(t, err)
	for _, call := range calls() {
		if strings.HasPrefix(call, "pr create ") && strings.Contains(call, "--title "+subject+" --body") {
			return
		}
	}
	t.Fatalf("PR title changed: %v", calls())
}
