package app

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRestackRefusesBranchCheckedOutByOperator(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	task := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	// Retire the managed worktree and put its branch in the main checkout.
	git(t, a.Root, "checkout", "-q", task.Branch)
	old := git(t, a.Root, "rev-parse", "HEAD")
	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	write(t, other, "main.txt", "main\n")
	commitAll(t, other, "main moves")
	git(t, other, "push", "-q", "origin", "main")
	_, err := a.Restack()
	if err == nil || !strings.Contains(err.Error(), "checked out") || !strings.Contains(err.Error(), a.Root) {
		t.Fatalf("restack must report operator checkout: %v", err)
	}
	if got := git(t, a.Root, "rev-parse", "HEAD"); got != old {
		t.Fatalf("HEAD moved: %s", got)
	}
	if status := git(t, a.Root, "status", "--porcelain"); status != "" {
		t.Fatalf("worktree became dirty: %s", status)
	}
}

func TestRestackRefusesAdditionalCheckout(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, _ := originWithGh(t, a)
	task := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	checkout := filepath.Join(t.TempDir(), "operator checkout")
	git(t, a.Root, "worktree", "add", checkout, task.Branch)
	old := git(t, checkout, "rev-parse", "HEAD")
	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	write(t, other, "main.txt", "main\n")
	commitAll(t, other, "main moves")
	git(t, other, "push", "-q", "origin", "main")
	_, err := a.Restack()
	if err == nil || !strings.Contains(err.Error(), checkout) {
		t.Fatalf("missing additional checkout: %v", err)
	}
	if got := git(t, checkout, "rev-parse", "HEAD"); got != old {
		t.Fatalf("checkout moved: %s", got)
	}
}
