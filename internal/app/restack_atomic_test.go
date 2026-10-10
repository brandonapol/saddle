package app

import (
	"testing"
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
	if err == nil { t.Fatal("expected compare-and-swap failure") }
	if got := git(t, a.Root, "rev-parse", landed[0].Branch); got != landed[0].To { t.Fatalf("first ref moved on failed transaction: %s", got) }
	if got := git(t, a.Root, "rev-parse", a.Cfg.Integration); got != old { t.Fatalf("integration moved: %s", got) }
}
