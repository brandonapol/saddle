package cli

import (
	"os"
	"strings"
	"testing"
)

// #220: saddle publish prints an existing PR, else pushes and opens one with
// the flags given.
func TestPublishCommand(t *testing.T) {
	a, log := landedPR(t)
	t.Chdir(a.Root)

	if out := run(t, "publish", "t1"); !strings.Contains(out, "t1 already has a PR: https://github.com/o/r/pull/1") {
		t.Fatalf("publish with a PR open:\n%s", out)
	}
	if err := a.Store.SetField("t1", "pr", ""); err != nil {
		t.Fatal(err)
	}
	out := run(t, "publish", "t1", "--branch-name", "fix/one", "--base", "main", "--draft")
	if !strings.Contains(out, "published t1 (1 commit) as fix/one: https://github.com/o/r/pull/1") {
		t.Fatalf("publish:\n%s", out)
	}
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "pr create --base main --head fix/one") || !strings.Contains(string(b), "--draft") {
		t.Fatalf("gh calls:\n%s", b)
	}
}
