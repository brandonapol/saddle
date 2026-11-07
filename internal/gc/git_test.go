package gc

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func repo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "init", "-q", "-b", "main")
	run(t, dir, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "root")
	return dir
}

// #802: only the newest backup refs stay; the backups are named by when
// they were taken, so the newest sort last.
func TestPruneBackupsKeepsNewest(t *testing.T) {
	t.Parallel()
	dir := repo(t)
	for i := range 5 {
		run(t, dir, "update-ref", fmt.Sprintf("refs/saddle/integration-backups/20261101T00000%dZ-abc", i), "HEAD")
	}
	run(t, dir, "update-ref", "refs/saddle/wip/t1", "HEAD")

	gone, err := PruneBackups(dir, BackupPrefix, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(gone) != 3 {
		t.Fatalf("pruned %v, want the 3 oldest", gone)
	}
	left := run(t, dir, "for-each-ref", "--format=%(refname)", BackupPrefix)
	want := BackupPrefix + "20261101T000003Z-abc\n" + BackupPrefix + "20261101T000004Z-abc"
	if left != want {
		t.Fatalf("left %q, want %q", left, want)
	}
	if run(t, dir, "for-each-ref", "refs/saddle/wip/") == "" {
		t.Fatal("pruned a wip ref")
	}
}

func looseCount(t *testing.T, dir string) int {
	t.Helper()
	for _, l := range strings.Split(run(t, dir, "count-objects", "-v"), "\n") {
		if v, ok := strings.CutPrefix(l, "count: "); ok {
			n, _ := strconv.Atoi(v)
			return n
		}
	}
	t.Fatal("no count in count-objects")
	return 0
}

// #802: loose objects are packed even with gc.auto=0, as on quark.
func TestMaintainPacksLooseObjects(t *testing.T) {
	t.Parallel()
	dir := repo(t)
	run(t, dir, "config", "gc.auto", "0")
	for i := range 150 {
		cmd := exec.Command("git", "-C", dir, "hash-object", "-w", "--stdin")
		cmd.Stdin = strings.NewReader(fmt.Sprint("blob ", i))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	before := looseCount(t, dir)
	if err := Maintain(dir); err != nil {
		t.Fatal(err)
	}
	if after := looseCount(t, dir); after >= before {
		t.Fatalf("loose objects %d -> %d, want fewer", before, after)
	}
}
