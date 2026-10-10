package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

func TestRestackRefTransactionLeavesAllRefsOnCASFailure(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	_, _ = originWithGh(t, a)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	landed, err := a.landedStack()
	must(t, err)
	old := git(t, a.Root, "rev-parse", a.Cfg.Integration)
	base := git(t, a.Root, "rev-parse", "main")
	plan := []restacked{
		{landedTask: landed[0], NewFrom: base, NewTo: old},
		{landedTask: landed[1], NewFrom: old, NewTo: landed[0].To},
	}
	// The second ref changed concurrently after planning. The first must stay
	// put too; individual update-ref calls leave a half-applied stack.
	asTrain(t, a.Root, "update-ref", "refs/heads/"+landed[1].Branch, base)
	var res RestackResult
	err = a.moveStack(plan, old, base, &res)
	if err == nil {
		t.Fatal("expected compare-and-swap failure")
	}
	if got := git(t, a.Root, "rev-parse", landed[0].Branch); got != landed[0].To {
		t.Fatalf("first ref moved on failed transaction: %s", got)
	}
	if got := git(t, a.Root, "rev-parse", a.Cfg.Integration); got != old {
		t.Fatalf("integration moved: %s", got)
	}
}

func TestRestackTransactionHookFailureLeavesEveryRef(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	landed, err := a.landedStack()
	must(t, err)
	// The hook accepts t1 alone but rejects t2. A sequence of updates would
	// move t1 before failing; one transaction keeps both old tips.
	hook := "#!/bin/sh\n[ \"$1\" != prepared ] && exit 0\ninput=$(cat)\ncase \"$input\" in *refs/heads/" + landed[1].Branch + "*) exit 1;; esac\nexit 0\n"
	must(t, os.WriteFile(filepath.Join(a.Root, ".git", "hooks", "reference-transaction"), []byte(hook), 0o755))
	moves := []RestackMove{
		{Ref: "refs/heads/" + landed[0].Branch, Old: landed[0].To, New: landed[1].To},
		{Ref: "refs/heads/" + landed[1].Branch, Old: landed[1].To, New: landed[0].To},
	}
	if err := a.moveRestackRefs(moves, false); err == nil {
		t.Fatal("hook must reject transaction")
	}
	for _, move := range moves {
		if got := git(t, a.Root, "rev-parse", move.Ref); got != move.Old {
			t.Fatalf("%s moved: %s", move.Ref, got)
		}
	}
}

func TestInterruptedRestackCanContinueOrAbort(t *testing.T) {
	for _, committed := range []bool{false, true} {
		for _, abort := range []bool{false, true} {
			t.Run(fmt.Sprintf("committed=%t/abort=%t", committed, abort), func(t *testing.T) {
				a := trainSetup(t)
				a.Cfg.CloseOnLand = false
				task := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
				landed, err := a.landedStack()
				must(t, err)
				old := landed[0].To
				base := git(t, a.Root, "rev-parse", "main")
				next := git(t, a.Root, "commit-tree", old+"^{tree}", "-p", base, "-m", "rebuilt one")
				j := restackJournal{Plan: []restacked{{landedTask: landed[0], NewFrom: base, NewTo: next}}, Moves: []RestackMove{
					{Task: task.ID, Ref: "refs/heads/" + task.Branch, Old: old, New: next},
					{Ref: "refs/heads/" + a.Cfg.Integration, Old: old, New: next},
				}, Checkouts: []store.Task{task}}
				must(t, a.writeRestackJournal(j))
				if !RestackInterrupted(a.Root) {
					t.Fatal("interruption not reported")
				}
				if _, err := a.PRs(); err == nil || !strings.Contains(err.Error(), "--continue") {
					t.Fatalf("publish must wait for recovery: %v", err)
				}
				// Simulate process death after detaching a checkout, optionally
				// after Git committed but before SQLite or checkout refresh.
				asTrain(t, task.Worktree, "checkout", "-q", "--detach")
				if committed {
					must(t, a.moveRestackRefs(j.Moves, false))
				}
				must(t, a.RecoverRestack(abort))
				want := next
				if abort {
					want = old
				}
				for _, ref := range []string{task.Branch, a.Cfg.Integration} {
					if got := git(t, a.Root, "rev-parse", ref); got != want {
						t.Fatalf("%s=%s, want %s", ref, got, want)
					}
				}
				if got := git(t, task.Worktree, "rev-parse", "HEAD"); got != want {
					t.Fatalf("checkout HEAD=%s", got)
				}
				if status := git(t, task.Worktree, "status", "--porcelain"); status != "" {
					t.Fatalf("dirty checkout: %s", status)
				}
				if RestackInterrupted(a.Root) {
					t.Fatal("journal not cleared")
				}
			})
		}
	}
}
