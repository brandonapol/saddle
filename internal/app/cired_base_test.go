package app

import (
	"slices"
	"testing"
)

// saddle/base red only means "the PR below hasn't merged yet" (#838). It is
// never red CI: screening drops it, so it can't hold layers, spawn repairs
// or park a task as needs-human, and a layer that went red on it alone
// (before this fix) clears.
func TestSaddleBaseIsNeverCIRed(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	rerun := func(string) error { t.Fatal("reran saddle/base"); return nil }

	p := CIPoll{Task: "t2", PR: "2", Head: "aaa", Failed: []string{"saddle/base"}}
	got, err := a.ScreenCIPoll(p, nil, rerun)
	must(t, err)
	if len(got.Failed) != 0 {
		t.Fatalf("saddle/base still red: %+v", got)
	}

	p = CIPoll{Task: "t2", PR: "2", Head: "aaa", Failed: []string{"saddle/base", "CI / test"}}
	got, err = a.ScreenCIPoll(p, nil, rerun)
	must(t, err)
	if !slices.Equal(got.Failed, []string{"CI / test"}) {
		t.Fatalf("Failed = %v, want only the real check", got.Failed)
	}

	if !onlySaddleOwned([]string{"saddle/base"}) || onlySaddleOwned([]string{"saddle/base", "CI / test"}) || onlySaddleOwned(nil) {
		t.Fatal("onlySaddleOwned misclassifies")
	}
}
