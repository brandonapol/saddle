package app

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// prBases maps each branch the fake gh opened a PR for to its --base.
func prBases(log []string) map[string]string {
	out := map[string]string{}
	for _, c := range log {
		if !strings.HasPrefix(c, "pr create") {
			continue
		}
		f := strings.Fields(c)
		var base, head string
		for i := 0; i+1 < len(f); i++ {
			switch f[i] {
			case "--base":
				base = f[i+1]
			case "--head":
				head = f[i+1]
			}
		}
		out[head] = base
	}
	return out
}

// #52: unrelated tasks don't form one linear stack. A task that touches
// nothing an earlier one touched gets a PR on base holding only its own
// work; one that changes an earlier task's files, or shares its issue,
// stacks on it. Local branches stay at the commits the train landed.
func TestPRsSplitsUnrelatedTasksIntoStacks(t *testing.T) {
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	main := git(t, a.Root, "rev-parse", "main")
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	t3 := landTask(t, a, "t3", "one more", map[string]string{"one.txt": "one\nmore\n"})
	landed := map[string]string{}
	for _, tk := range []store.Task{t1, t2, t3} {
		landed[tk.ID] = git(t, a.Root, "rev-parse", tk.Branch)
	}

	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	bases := prBases(ghLog())
	want := map[string]string{t1.Branch: "main", t2.Branch: "main", t3.Branch: t1.Branch}
	for br, b := range want {
		if bases[br] != b {
			t.Fatalf("PR bases = %v, want %v", bases, want)
		}
	}
	// t1 publishes what landed; t2 and t3 publish their own commits replayed
	// onto main and onto t1.
	if got := remoteRev(t, origin, t1.Branch); got != landed[t1.ID] {
		t.Fatalf("t1 remote = %s, landed %s", got, landed[t1.ID])
	}
	r2 := remoteRev(t, origin, t2.Branch)
	if parent := git(t, origin, "rev-parse", r2+"^"); parent != main {
		t.Fatalf("t2's PR head is not on main: parent %s", parent)
	}
	if files := git(t, origin, "ls-tree", "-r", "--name-only", r2); strings.Contains(files, "one.txt") {
		t.Fatalf("t2's PR carries t1's work:\n%s", files)
	}
	r3 := remoteRev(t, origin, t3.Branch)
	if parent := git(t, origin, "rev-parse", r3+"^"); parent != landed[t1.ID] {
		t.Fatalf("t3's PR head is not on t1: parent %s", parent)
	}
	if files := git(t, origin, "ls-tree", "-r", "--name-only", r3); strings.Contains(files, "two.txt") {
		t.Fatalf("t3's PR carries t2's work:\n%s", files)
	}
	// The train's record is untouched: no drift, the stack checks clean.
	if d, err := a.Drift(); err != nil || len(d) > 0 {
		t.Fatalf("drift = %v, %v", d, err)
	}
	ls, err := a.StackLayers()
	must(t, err)
	for _, l := range ls {
		if l.Problem != "" {
			t.Fatalf("%s: %s", l.Task.ID, l.Problem)
		}
	}
	// Publishing again replays to the same commits, so nothing is force-pushed.
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	if got := remoteRev(t, origin, t2.Branch); got != r2 {
		t.Fatalf("t2 republished as %s, was %s", got, r2)
	}
}

// Tasks for the same issue stack even when their files don't overlap.
func TestPRsStacksTasksSharingAnIssue(t *testing.T) {
	a := trainSetup(t)
	_, ghLog := originWithGh(t, a)
	land := func(id, file string) store.Task {
		tk, err := a.Spawn(SpawnReq{ID: id, Title: id, Issue: 52})
		must(t, err)
		write(t, tk.Worktree, file, id+"\n")
		commitAll(t, tk.Worktree, id)
		must(t, a.Done(tk.ID, id))
		_, err = a.Land()
		must(t, err)
		return tk
	}
	t1, t2 := land("t1", "one.txt"), land("t2", "two.txt")
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	if b := prBases(ghLog())[t2.Branch]; b != t1.Branch {
		t.Fatalf("t2 base = %q, want %s", b, t1.Branch)
	}
}

// Serial files don't link tasks, but when a task's commits don't replay onto
// base without the work below it, it stacks there after all.
func TestPRsStackWhenReplayConflicts(t *testing.T) {
	a := trainSetup(t)
	a.Cfg.Serial = []string{"go.sum"}
	write(t, a.Root, "go.sum", "a\n")
	commitAll(t, a.Root, "go.sum")
	_, ghLog := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n", "go.sum": "a\nb\n"})
	t2 := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n", "go.sum": "a\nb\nc\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	if b := prBases(ghLog())[t2.Branch]; b != t1.Branch {
		t.Fatalf("t2 base = %q, want %s", b, t1.Branch)
	}
}

// output = "single" keeps the one linear stack.
func TestPRsSingleOutputKeepsOneStack(t *testing.T) {
	a := trainSetup(t)
	a.Cfg.Train.Output = "single"
	_, ghLog := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	if b := prBases(ghLog())[t2.Branch]; b != t1.Branch {
		t.Fatalf("t2 base = %q, want %s", b, t1.Branch)
	}
}

// Restack keeps the clustered layout: after main moves, unrelated tasks'
// PRs still target main, each holding only its own work on the new main, and
// a dependent one still targets its stack's branch.
func TestRestackKeepsClusteredLayout(t *testing.T) {
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	t3 := landTask(t, a, "t3", "one more", map[string]string{"one.txt": "one\nmore\n"})
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	before := len(ghLog())

	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	write(t, other, "main.txt", "main\n")
	commitAll(t, other, "main moves")
	git(t, other, "push", "-q", "origin", "main")
	main := git(t, other, "rev-parse", "HEAD")

	if _, err := a.Restack(); err != nil {
		t.Fatal(err)
	}
	edits := map[string]string{}
	for _, c := range ghLog()[before:] {
		if f := strings.Fields(c); len(f) == 5 && f[0] == "pr" && f[1] == "edit" && f[3] == "--base" {
			edits[f[2]] = f[4]
		}
	}
	for _, tk := range []store.Task{t1, t2, t3} {
		tk, _ = a.Store.Task(tk.ID)
		want := "main"
		if tk.ID == t3.ID {
			want = t1.Branch
		}
		if edits[tk.PR] != want {
			t.Fatalf("retargets = %v; %s want %s", edits, tk.ID, want)
		}
	}
	r2 := remoteRev(t, origin, t2.Branch)
	if parent := git(t, origin, "rev-parse", r2+"^"); parent != main {
		t.Fatalf("t2's PR head is not on the new main: parent %s", parent)
	}
	if files := git(t, origin, "ls-tree", "-r", "--name-only", r2); strings.Contains(files, "one.txt") {
		t.Fatalf("t2's PR carries t1's work:\n%s", files)
	}
	if got := remoteRev(t, origin, t1.Branch); got != git(t, a.Root, "rev-parse", t1.Branch) {
		t.Fatalf("t1 remote %s isn't its restacked landed commit", got)
	}
}
