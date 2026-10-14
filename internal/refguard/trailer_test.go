package refguard

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/lintgate"
)

// trailers are the Saddle-Task trailers of dir's HEAD commit.
func trailers(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(mustGit(t, dir, "log", "-1", "--format=%(trailers:key=Saddle-Task,valueonly)"))
}

// #331: every task commit carries a Saddle-Task trailer, worked out from the
// worktree's branch rather than baked in at install, so one shared hook
// serves every worktree. The repo's own commit-msg still runs after it and
// can still reject.
func TestCommitMsgAddsTaskTrailerAndChainsRepoHook(t *testing.T) {
	root, wt, _ := hookRepo(t)
	if err := Install(root, "/nonexistent/saddle"); err != nil {
		t.Fatal(err)
	}
	if out, err := commitFile(t, wt, "a.txt", "a\n\nCo-Authored-By: Claude <noreply@anthropic.com>"); err == nil || !strings.Contains(out, "no AI attribution") {
		t.Fatalf("the repo's commit-msg didn't run or didn't reject: %v\n%s", err, out)
	}
	mustGit(t, wt, "reset", "-q")
	if out, err := commitFile(t, wt, "a.txt", "a"); err != nil {
		t.Fatalf("clean commit: %v\n%s", err, out)
	}
	if got := trailers(t, wt); got != "t1" {
		t.Fatalf("worktree commit trailer = %q, want t1", got)
	}
	if out, err := gitIn(t, wt, nil, "commit", "-q", "--amend", "--no-edit"); err != nil || trailers(t, wt) != "t1" {
		t.Fatalf("amend duplicated or lost the trailer: %v %q\n%s", err, trailers(t, wt), out)
	}

	// The same shared hook names a second worktree's own task.
	wt2 := filepath.Join(t.TempDir(), "wt2")
	mustGit(t, root, "worktree", "add", "-q", "-b", "saddle/t22-y", wt2)
	if out, err := commitFile(t, wt2, "b.txt", "b"); err != nil || trailers(t, wt2) != "t22" {
		t.Fatalf("second worktree trailer = %q: %v\n%s", trailers(t, wt2), err, out)
	}

	// Off a task branch, SADDLE_TASK names the task; the orchestrator, the
	// train and plain checkouts add nothing.
	if out, err := commitFile(t, root, "c.txt", "c", "SADDLE_TASK=t7"); err != nil || trailers(t, root) != "t7" {
		t.Fatalf("SADDLE_TASK trailer = %q: %v\n%s", trailers(t, root), err, out)
	}
	for i, env := range [][]string{{"SADDLE_TASK=t0"}, {"SADDLE_TASK="}} {
		if out, err := commitFile(t, root, fmt.Sprintf("d%d.txt", i), strings.Join(env, ""), env...); err != nil || trailers(t, root) != "" {
			t.Fatalf("%v: trailer %q: %v\n%s", env, trailers(t, root), err, out)
		}
	}
	if out, err := commitFile(t, wt, "e.txt", "e", "SADDLE_TRAIN=1"); err != nil || trailers(t, wt) != "" {
		t.Fatalf("train commit trailer = %q: %v\n%s", trailers(t, wt), err, out)
	}
	hooks := filepath.Join(root, ".git", "hooks")
	if b, err := os.ReadFile(filepath.Join(hooks, "commit-msg"+lintgate.ChainSuffix)); err != nil || !strings.Contains(string(b), lintgate.RepoHookMarker) {
		t.Fatalf("the repo's commit-msg wrapper isn't in the chained slot: %v", err)
	}
}

// A commit-msg someone installed by hand, or the wrapper an older saddle
// wrote in commit-msg itself, is chained, not clobbered.
func TestCommitMsgChainsExistingHook(t *testing.T) {
	root, wt, _ := hookRepo(t)
	hooks := filepath.Join(root, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	mine := "#!/bin/sh\ngrep -q nope \"$1\" && { echo 'mine says no'; exit 1; }\nexit 0\n"
	if err := os.WriteFile(filepath.Join(hooks, "commit-msg"), []byte(mine), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Install(root, "/nonexistent/saddle"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(hooks, "commit-msg"+lintgate.ChainSuffix)); string(b) != mine {
		t.Fatalf("foreign commit-msg not chained:\n%s", b)
	}
	if out, err := commitFile(t, wt, "a.txt", "nope"); err == nil || !strings.Contains(out, "mine says no") {
		t.Fatalf("chained commit-msg didn't reject: %v\n%s", err, out)
	}
	mustGit(t, wt, "reset", "-q")
	if out, err := commitFile(t, wt, "a.txt", "fine"); err != nil || trailers(t, wt) != "t1" {
		t.Fatalf("trailer %q: %v\n%s", trailers(t, wt), err, out)
	}
	// Reinstalling leaves the chain as it is.
	if err := Install(root, "/nonexistent/saddle"); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(hooks, "commit-msg"+lintgate.ChainSuffix)); string(b) != mine {
		t.Fatalf("reinstall moved the chain:\n%s", b)
	}
}
