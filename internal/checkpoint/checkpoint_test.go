package checkpoint

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// repo makes a repo with one commit on main and a worktree on saddle/t1-x.
func repo(t *testing.T) (root, wt string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root = t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	write(t, root, "a.txt", "a\n")
	git(t, root, "add", "a.txt")
	git(t, root, "commit", "-qm", "init")
	wt = filepath.Join(t.TempDir(), "t1")
	git(t, root, "worktree", "add", "-q", "-b", "saddle/t1-x", wt)
	return root, wt
}

func TestTakeSnapshotsWorkingTreeWithoutTouchingBranchOrIndex(t *testing.T) {
	root, wt := repo(t)
	head := git(t, wt, "rev-parse", "HEAD")
	write(t, wt, "a.txt", "a\nstaged\n")
	git(t, wt, "add", "a.txt")
	write(t, wt, "a.txt", "a\nstaged\nunstaged\n")
	write(t, wt, "new.txt", "untracked\n")
	status := git(t, wt, "status", "--porcelain")
	cached := git(t, wt, "diff", "--cached")

	c, err := Take(root, "t1", wt)
	if err != nil || c == "" {
		t.Fatalf("Take = %q, %v", c, err)
	}
	if got := git(t, root, "rev-parse", Ref("t1")); got != c {
		t.Fatalf("%s = %s, want %s", Ref("t1"), got, c)
	}
	if got := git(t, root, "rev-parse", c+"^"); got != head {
		t.Fatalf("checkpoint parent %s, want HEAD %s", got, head)
	}
	if got := git(t, root, "show", c+":a.txt"); got != "a\nstaged\nunstaged" {
		t.Fatalf("a.txt in checkpoint = %q", got)
	}
	if got := git(t, root, "show", c+":new.txt"); got != "untracked" {
		t.Fatalf("new.txt in checkpoint = %q", got)
	}
	if got := git(t, wt, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD moved to %s", got)
	}
	if got := git(t, root, "rev-parse", "saddle/t1-x"); got != head {
		t.Fatalf("branch moved to %s", got)
	}
	if got := git(t, wt, "status", "--porcelain"); got != status {
		t.Fatalf("status changed:\n%s\nwant\n%s", got, status)
	}
	if got := git(t, wt, "diff", "--cached"); got != cached {
		t.Fatalf("index changed:\n%s", got)
	}
}

func TestTakeSkipsCleanAndUnchangedTrees(t *testing.T) {
	root, wt := repo(t)
	if c, err := Take(root, "t1", wt); err != nil || c != "" {
		t.Fatalf("clean: Take = %q, %v", c, err)
	}
	if _, err := Lookup(root, "t1"); !errors.Is(err, ErrNone) {
		t.Fatalf("clean tree wrote a checkpoint: %v", err)
	}
	write(t, wt, "b.txt", "b\n")
	first, err := Take(root, "t1", wt)
	if err != nil || first == "" {
		t.Fatalf("dirty: Take = %q, %v", first, err)
	}
	if c, err := Take(root, "t1", wt); err != nil || c != "" {
		t.Fatalf("unchanged: Take = %q, %v", c, err)
	}
	write(t, wt, "b.txt", "b2\n")
	second, err := Take(root, "t1", wt)
	if err != nil || second == "" || second == first {
		t.Fatalf("changed: Take = %q, %v (first %s)", second, err, first)
	}
	// A commit moves HEAD; the same tree on a new HEAD is a new checkpoint
	// only once the tree differs from that HEAD.
	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-qm", "b")
	if c, err := Take(root, "t1", wt); err != nil || c != "" {
		t.Fatalf("committed: Take = %q, %v", c, err)
	}
}

func TestTakeMissingWorktreeIsNoop(t *testing.T) {
	root, _ := repo(t)
	if c, err := Take(root, "t9", filepath.Join(root, "gone")); err != nil || c != "" {
		t.Fatalf("Take = %q, %v", c, err)
	}
}

func TestPruneAndList(t *testing.T) {
	root, wt := repo(t)
	write(t, wt, "b.txt", "b\n")
	c, err := Take(root, "t1", wt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := List(root)
	if err != nil || len(got) != 1 || got["t1"] != c {
		t.Fatalf("List = %v, %v", got, err)
	}
	if err := Prune(root, "t1"); err != nil {
		t.Fatal(err)
	}
	if err := Prune(root, "t1"); err != nil {
		t.Fatalf("pruning twice: %v", err)
	}
	if got, err := List(root); err != nil || len(got) != 0 {
		t.Fatalf("after prune List = %v, %v", got, err)
	}
}

func TestCheckpointRefsAreOutsideBranchNamespaces(t *testing.T) {
	r := Ref("t1")
	if strings.HasPrefix(r, "refs/heads/") || strings.HasPrefix(r, "refs/tags/") || strings.HasPrefix(r, "refs/remotes/") {
		t.Fatalf("%s is in a namespace git pushes or the ref guard watches", r)
	}
}

type fakeHost struct {
	targets []Target
	nudges  []string
	events  []string
}

func (h *fakeHost) Targets() ([]Target, error) { return h.targets, nil }
func (h *fakeHost) Nudge(task, text string) error {
	h.nudges = append(h.nudges, task+": "+text)
	return nil
}
func (h *fakeHost) Event(task, kind, detail string) { h.events = append(h.events, task+" "+kind) }

func TestWatcherCheckpointsEveryInterval(t *testing.T) {
	root, wt := repo(t)
	h := &fakeHost{targets: []Target{{Task: "t1", Worktree: wt}}}
	now := time.Unix(1000, 0)
	w := NewWatcher(root, h, Policy{Every: time.Minute})
	w.now = func() time.Time { return now }

	write(t, wt, "b.txt", "b\n")
	w.Tick()
	first, err := Lookup(root, "t1")
	if err != nil {
		t.Fatalf("first tick wrote no checkpoint: %v", err)
	}
	write(t, wt, "b.txt", "b2\n")
	now = now.Add(30 * time.Second)
	w.Tick()
	if c, _ := Lookup(root, "t1"); c != first {
		t.Fatal("checkpointed again before the interval")
	}
	now = now.Add(31 * time.Second)
	w.Tick()
	if c, _ := Lookup(root, "t1"); c == first {
		t.Fatal("no new checkpoint after the interval")
	}
}

func TestWatcherNudgesOnceUntilTheAgentCommits(t *testing.T) {
	root, wt := repo(t)
	h := &fakeHost{targets: []Target{{Task: "t1", Worktree: wt}}}
	now := time.Unix(1000, 0)
	w := NewWatcher(root, h, Policy{Every: time.Minute, NudgeFiles: 2, NudgeAfter: 10 * time.Minute})
	w.now = func() time.Time { return now }

	write(t, wt, "b.txt", "b\n")
	w.Tick()
	if len(h.nudges) != 0 {
		t.Fatalf("one dirty file nudged: %v", h.nudges)
	}
	write(t, wt, "c.txt", "c\n")
	w.Tick()
	if len(h.nudges) != 1 || !strings.Contains(h.nudges[0], "commit") {
		t.Fatalf("two dirty files: nudges = %v", h.nudges)
	}
	write(t, wt, "d.txt", "d\n")
	now = now.Add(time.Hour)
	w.Tick()
	if len(h.nudges) != 1 {
		t.Fatalf("nudged again before a commit: %v", h.nudges)
	}

	git(t, wt, "add", "-A")
	git(t, wt, "commit", "-qm", "bcd")
	w.Tick()
	write(t, wt, "e.txt", "e\n")
	w.Tick()
	if len(h.nudges) != 1 {
		t.Fatalf("one file after a commit nudged: %v", h.nudges)
	}
	// Dirty too long without a commit nudges, however few files.
	now = now.Add(11 * time.Minute)
	w.Tick()
	if len(h.nudges) != 2 {
		t.Fatalf("dirty for 11m: nudges = %v", h.nudges)
	}
}

func TestWatcherForgetsTasksThatLeave(t *testing.T) {
	root, wt := repo(t)
	h := &fakeHost{targets: []Target{{Task: "t1", Worktree: wt}}}
	w := NewWatcher(root, h, Policy{Every: time.Minute, NudgeFiles: 1})
	write(t, wt, "b.txt", "b\n")
	w.Tick()
	h.targets = nil
	w.Tick()
	if len(w.state) != 0 {
		t.Fatalf("state kept for a task that left: %v", w.state)
	}
}

// Untracked files in a new directory count one by one, not as the one
// "?? dir/" line git status shows by default.
func TestWatcherCountsFilesInsideNewDirectories(t *testing.T) {
	root, wt := repo(t)
	h := &fakeHost{targets: []Target{{Task: "t1", Worktree: wt}}}
	w := NewWatcher(root, h, Policy{NudgeFiles: 3})
	if err := os.Mkdir(filepath.Join(wt, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"x", "y", "z"} {
		write(t, wt, "pkg/"+f+".txt", f+"\n")
	}
	w.Tick()
	if len(h.nudges) != 1 {
		t.Fatalf("three new files in pkg/: nudges = %v", h.nudges)
	}
}
