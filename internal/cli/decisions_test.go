package cli

import (
	"strings"
	"testing"
)

// #464: saddle decisions files a card, lists it, answers it by id, and
// allow pre-authorizes a class so the next one never waits.
func TestDecisionsCommand(t *testing.T) {
	a := automergeRepo(t)
	t.Chdir(a.Root)
	t.Setenv("SADDLE_TASK", "")

	out, err := runCmd(t, decisionsCmd())
	if err != nil || !strings.Contains(out, "No decisions") {
		t.Fatalf("empty: %v\n%s", err, out)
	}
	out, err = runCmd(t, decisionsCmd(), "file", "--task", "t1", "--class", "release-tag", "--subject", "v1.0.0", "--irreversible",
		"--option", "Push it: git push origin v1.0.0", "--option", "Hold it: nothing", "Push the release tag v1.0.0?")
	if err != nil || !strings.Contains(out, "[owner:t1|release-tag|v1.0.0]") || !strings.Contains(out, "never runs it without you") {
		t.Fatalf("file: %v\n%s", err, out)
	}
	if _, err := runCmd(t, decisionsCmd(), "file", "--option", "no colon", "q?"); err == nil {
		t.Fatal("a malformed option filed")
	}
	out, err = runCmd(t, decisionsCmd())
	if err != nil || !strings.Contains(out, "Push the release tag v1.0.0?") {
		t.Fatalf("list: %v\n%s", err, out)
	}
	out, err = runCmd(t, decisionsCmd(), "digest")
	if err != nil || strings.Count(out, "owner:t1|release-tag|v1.0.0") != 1 {
		t.Fatalf("digest: %v\n%s", err, out)
	}
	out, err = runCmd(t, decisionsCmd(), "answer", "owner:t1|release-tag|v1.0.0", "2")
	if err != nil || !strings.Contains(out, "you chose option 2 (Hold it)") {
		t.Fatalf("answer: %v\n%s", err, out)
	}

	out, err = runCmd(t, decisionsCmd(), "allow", "release-tag", "v*", "--note", "tags are fine")
	if err != nil || !strings.Contains(out, `[[preauth]] release-tag "v*" (tags are fine)`) {
		t.Fatalf("allow: %v\n%s", err, out)
	}
	out, err = runCmd(t, decisionsCmd(), "file", "--task", "t1", "--class", "release-tag", "--subject", "v1.0.1", "--irreversible",
		"--option", "Push it: git push origin v1.0.1", "Push the release tag v1.0.1?")
	if err != nil || !strings.Contains(out, "pre-authorized, so saddle ran option 1 (Push it)") {
		t.Fatalf("pre-authorized file: %v\n%s", err, out)
	}
}
