package cli

import (
	"strings"
	"testing"
)

// #332: saddle automerge approve is registered and needs the PR's head, so
// it refuses when GitHub can't be read rather than approving blind.
func TestAutomergeApproveCommand(t *testing.T) {
	if c, _, err := Root().Find([]string{"automerge", "approve"}); err != nil || c.Name() != "approve" {
		t.Fatalf("saddle automerge approve not registered: %v", err)
	}
	automergeRepo(t)
	if _, err := runCmd(t, automergeCmd(), "approve"); err == nil {
		t.Fatal("approve with no ref: want error")
	}
	out, err := runCmd(t, automergeCmd(), "approve", "t1")
	if err == nil || !strings.Contains(err.Error(), "isn't an open PR or stack") {
		t.Fatalf("approve of a task with no PR: %v\n%s", err, out)
	}
}
