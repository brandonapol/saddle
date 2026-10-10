package gitx

import (
	"strings"
	"testing"
)

func TestRefUpdatesHaveSaddleReflog(t *testing.T) {
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "commit", "-q", "--allow-empty", "-m", "one")
	gitT(t, dir, "commit", "-q", "--allow-empty", "-m", "two")
	gitT(t, dir, "branch", "task", "HEAD~")
	if _, err := Run(dir, "update-ref", "refs/heads/task", "HEAD"); err != nil {
		t.Fatal(err)
	}
	out := gitT(t, dir, "reflog", "-1", "--format=%gs", "task")
	if !strings.Contains(out, "saddle:") {
		t.Fatalf("unattributed ref move: %q", out)
	}
}
