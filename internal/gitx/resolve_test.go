package gitx_test

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/gitx"
)

func TestResolveCommitsInOneProcess(t *testing.T) {
	root, _ := setup(t)
	head := strings.TrimSpace(run(t, root, "rev-parse", "HEAD"))
	tree := strings.TrimSpace(run(t, root, "rev-parse", "HEAD^{tree}"))

	before := gitx.Calls()
	got, err := gitx.ResolveCommits(root, []string{"refs/heads/integration", "refs/heads/nope", head, tree, "task"})
	if err != nil {
		t.Fatal(err)
	}
	if n := gitx.Calls() - before; n != 1 {
		t.Fatalf("started %d git processes, want 1", n)
	}
	want := map[string]string{"refs/heads/integration": head, head: head, "task": head}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v (a missing ref and a tree are left out)", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q", k, got[k], v)
		}
	}

	before = gitx.Calls()
	if got, err := gitx.ResolveCommits(root, nil); err != nil || len(got) != 0 || gitx.Calls() != before {
		t.Fatalf("no refs: %v, %v, and it ran git", got, err)
	}
}
