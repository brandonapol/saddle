package cli

import (
	"strings"
	"testing"
)

// repair refuses what it can't act on, naming why.
func TestRepairRefuses(t *testing.T) {
	a := automergeRepo(t) // t1 spawned, not landed
	t.Chdir(a.Root)
	if out, err := runCmd(t, Root(), "repair", "t1"); err == nil || !strings.Contains(err.Error(), "isn't in the merge train") {
		t.Fatalf("repair of an unqueued task: %v\n%s", err, out)
	}
	if out, err := runCmd(t, Root(), "repair", "t9"); err == nil || !strings.Contains(err.Error(), "no task") {
		t.Fatalf("repair of an unknown task: %v\n%s", err, out)
	}
}
