package app

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/store"
)

// ADR 0012, #932 and #933: a task's branch is cut from main (or its one
// unmerged after task's branch), sync rebases onto that, and done publishes
// the branch as one PR against it with only the task's own commits.

// doneGH gives the repo a bare origin holding main and a fake gh that keeps
// the PRs it opens: pr create records "base head url", pr list --head and pr
// view answer from them. It returns the origin and a reader of those PRs.
func doneGH(t *testing.T, a *App) (origin string, prs func() []string) {
	t.Helper()
	origin = t.TempDir()
	git(t, origin, "init", "-q", "--bare", "-b", "main")
	git(t, a.Root, "remote", "add", "origin", origin)
	git(t, a.Root, "push", "-q", "-u", "origin", a.Cfg.Base)
	bin := filepath.Join(a.Root, ".git", FakeGHSubdir)
	must(t, os.MkdirAll(bin, 0o755))
	db := filepath.Join(bin, "prs")
	script := `#!/bin/sh
echo "$*" >> "` + bin + `/gh.log"
db="` + db + `"
touch "$db"
case "$1 $2" in
"pr create")
	n=$(( $(wc -l < "$db") + 1 ))
	echo "$4 $6 https://github.com/o/r/pull/$n" >> "$db"
	echo "https://github.com/o/r/pull/$n" ;;
"pr list")
	line=$(awk -v h="$6" '$2==h' "$db" | tail -1)
	if [ -n "$line" ]; then set -- $line; echo "[{\"url\":\"$3\",\"baseRefName\":\"$1\"}]"; else echo "[]"; fi ;;
"pr view")
	line=$(awk -v u="$3" '$3==u' "$db" | tail -1)
	set -- $line
	echo "{\"state\":\"OPEN\",\"baseRefName\":\"$1\",\"url\":\"$3\"}" ;;
esac
`
	must(t, os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755))
	return origin, func() []string {
		b, _ := os.ReadFile(db)
		if s := strings.TrimSpace(string(b)); s != "" {
			return strings.Split(s, "\n")
		}
		return nil
	}
}

// integrationAhead puts a commit on the integration branch that main lacks:
// another task's landed, unmerged work in the legacy model.
func integrationAhead(t *testing.T, a *App, file string) {
	t.Helper()
	must(t, a.ensureIntegration())
	blob := strings.TrimSpace(gitIn(t, a.Root, file+"\n", "hash-object", "-w", "--stdin"))
	base := git(t, a.Root, "rev-parse", a.Cfg.Integration)
	idx := filepath.Join(t.TempDir(), "index")
	env := []string{"GIT_INDEX_FILE=" + idx}
	gitEnv(t, a.Root, env, "read-tree", base)
	gitEnv(t, a.Root, env, "update-index", "--add", "--cacheinfo", "100644,"+blob+","+file)
	nt := strings.TrimSpace(gitEnv(t, a.Root, env, "write-tree"))
	c := strings.TrimSpace(git(t, a.Root, "commit-tree", nt, "-p", base, "-m", "landed elsewhere"))
	if _, err := trainGit(a.Root, "update-ref", "refs/heads/"+a.Cfg.Integration, c, base); err != nil {
		t.Fatal(err)
	}
}

// gitIn runs git in dir with stdin.
func gitIn(t *testing.T, dir, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return string(out)
}

// gitEnv runs git in dir with extra environment.
func gitEnv(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// commitFiles writes files in dir and commits them with msg.
func commitFiles(t *testing.T, dir, msg string, files map[string]string) string {
	t.Helper()
	for rel, body := range files {
		write(t, dir, rel, body)
	}
	commitAll(t, dir, msg)
	return git(t, dir, "rev-parse", "HEAD")
}

// #932: integration ahead of main (other tasks' landed, unmerged work) must
// not leak into a new task: its branch starts at main's tip.
func TestSpawnCutsFromMainNotIntegration(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	doneGH(t, a)
	// Integration carries a commit main doesn't have.
	integrationAhead(t, a, "landed.txt")
	tk, err := a.Spawn(SpawnReq{Title: "fresh", Parent: OrchestratorID, Issue: 7})
	must(t, err)
	mainTip := git(t, a.Root, "rev-parse", "main")
	if mb := git(t, a.Root, "merge-base", "main", tk.Branch); mb != mainTip {
		t.Fatalf("merge-base(main, %s) = %s, want main's tip %s", tk.Branch, mb, mainTip)
	}
	if out := git(t, a.Root, "log", "--oneline", "main.."+tk.Branch); out != "" {
		t.Fatalf("main..%s is not empty:\n%s", tk.Branch, out)
	}
	got, err := a.Store.Task(tk.ID)
	must(t, err)
	if got.Base != "main" {
		t.Fatalf("stored base = %q, want main", got.Base)
	}
}

// #932: after an unmerged task, the branch starts at that task's tip and its
// base is that branch.
func TestSpawnAfterUnmergedStartsAtItsBranch(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	doneGH(t, a)
	x, err := a.Spawn(SpawnReq{ID: "t1", Title: "dep"})
	must(t, err)
	tip := commitFiles(t, x.Worktree, "dep work", map[string]string{"dep.txt": "dep\n"})
	y, err := a.Spawn(SpawnReq{ID: "t2", Title: "child", After: []string{"t1"}})
	must(t, err)
	if got := git(t, a.Root, "rev-parse", y.Branch); got != tip {
		t.Fatalf("%s starts at %s, want t1's tip %s", y.Branch, got, tip)
	}
	if got, _ := a.Store.Task(y.ID); got.Base != x.Branch {
		t.Fatalf("stored base = %q, want %s", got.Base, x.Branch)
	}
}

// #932: two unmerged after tasks can't both be a branch's base: spawn says to
// split the task or wait, and leaves nothing behind.
func TestSpawnAfterTwoUnmergedRefused(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	for _, id := range []string{"t1", "t2"} {
		x, err := a.Spawn(SpawnReq{ID: id, Title: "dep " + id})
		must(t, err)
		commitFiles(t, x.Worktree, "work "+id, map[string]string{id + ".txt": id})
	}
	_, err := a.Spawn(SpawnReq{ID: "t3", Title: "child", After: []string{"t1", "t2"}})
	if err == nil || !strings.Contains(err.Error(), "t1 and t2") || !strings.Contains(err.Error(), "Split the task") {
		t.Fatalf("err = %v", err)
	}
	if _, err := a.Store.Task("t3"); err == nil {
		t.Fatal("a refused spawn left its task behind")
	}
}

// #932: sync after another task's work reaches main gains main's new commits,
// and nothing from unmerged tasks on integration.
func TestSyncRebasesOntoMainNotIntegration(t *testing.T) {
	// Not parallel: sync runs as the task (SADDLE_TASK).
	a, _ := setup(t)
	origin, _ := doneGH(t, a)
	tk, err := a.Spawn(SpawnReq{Title: "mine"})
	must(t, err)
	commitFiles(t, tk.Worktree, "mine", map[string]string{"mine.txt": "mine\n"})
	// Unmerged work on integration.
	integrationAhead(t, a, "unmerged.txt")
	// Another task's work merges into main on the remote.
	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	merged := commitFiles(t, other, "merged elsewhere", map[string]string{"merged.txt": "m\n"})
	git(t, other, "push", "-q", "origin", "main")

	t.Setenv("SADDLE_TASK", tk.ID)
	rr, err := a.Sync(tk.ID)
	must(t, err)
	if !rr.OK {
		t.Fatalf("sync: %+v", rr)
	}
	if _, err := os.Stat(filepath.Join(tk.Worktree, "merged.txt")); err != nil {
		t.Errorf("sync didn't bring main's new commit %s", short(merged))
	}
	if _, err := os.Stat(filepath.Join(tk.Worktree, "unmerged.txt")); err == nil {
		t.Error("sync brought integration's unmerged work into the branch")
	}
	if out := git(t, tk.Worktree, "log", "--format=%s", "origin/main..HEAD"); out != "mine" {
		t.Errorf("origin/main..HEAD = %q, want only the task's commit", out)
	}
}

// #933: a commit of another task in main..branch is refused at done, named;
// nothing is pushed and the task stays running for its agent to fix.
func TestDoneRefusesForeignCommit(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	origin, prs := doneGH(t, a)
	tk, err := a.Spawn(SpawnReq{ID: "t1", Title: "mine"})
	must(t, err)
	commitFiles(t, tk.Worktree, "mine", map[string]string{"mine.txt": "mine\n"})
	// Another task's commit, as a cherry-pick carries it: its own trailer,
	// no commit-msg hook run here.
	write(t, tk.Worktree, "theirs.txt", "theirs\n")
	git(t, tk.Worktree, "add", "-A")
	git(t, tk.Worktree, "commit", "-q", "--no-verify", "-m", "theirs\n\nSaddle-Task: t9")
	foreign := git(t, tk.Worktree, "rev-parse", "HEAD")

	err = a.Done(tk.ID, "mine")
	var refused *PublishRefusedError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), short(foreign)) || !strings.Contains(err.Error(), "theirs") {
		t.Fatalf("done = %v, want a refusal naming %s", err, short(foreign))
	}
	if got := remoteRev(t, origin, tk.Branch); got != "" {
		t.Fatalf("refused branch was pushed at %s", got)
	}
	if p := prs(); len(p) != 0 {
		t.Fatalf("refused branch got PRs: %v", p)
	}
	if got, _ := a.Store.Task(tk.ID); got.Status != store.Running {
		t.Fatalf("status = %s, want running", got.Status)
	}
	if es, _ := a.Store.Train(); len(es) != 0 {
		t.Fatalf("done enqueued into the train: %+v", es)
	}
}

// #933: a commit with no trailer that another task's branch holds is foreign
// too.
func TestDoneRefusesCommitFromAnotherTaskBranch(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	doneGH(t, a)
	x, err := a.Spawn(SpawnReq{ID: "t1", Title: "other"})
	must(t, err)
	theirs := commitFiles(t, x.Worktree, "other work", map[string]string{"other.txt": "o\n"})
	y, err := a.Spawn(SpawnReq{ID: "t2", Title: "mine"})
	must(t, err)
	git(t, y.Worktree, "merge", "-q", "--ff-only", x.Branch)
	commitFiles(t, y.Worktree, "mine", map[string]string{"mine.txt": "m\n"})
	err = a.Done(y.ID, "mine")
	if err == nil || !strings.Contains(err.Error(), short(theirs)) || !strings.Contains(err.Error(), "t1") {
		t.Fatalf("done = %v, want a refusal naming %s of t1", err, short(theirs))
	}
}

// #933: done pushes the branch and opens exactly one PR against main with
// only the task's commits; done again at the same head opens no second PR.
// No train entry, no integration branch, no status posted.
func TestDonePublishesOnePRAgainstMain(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	origin, prs := doneGH(t, a)
	tk, err := a.Spawn(SpawnReq{ID: "t1", Title: "mine", Issue: 12})
	must(t, err)
	commitFiles(t, tk.Worktree, "mine", map[string]string{"mine.txt": "mine\n"})
	integ := git(t, a.Root, "rev-parse", a.Cfg.Integration)
	must(t, a.Done(tk.ID, "did my thing"))

	got, err := a.Store.Task(tk.ID)
	must(t, err)
	if got.Status != store.PROpen || got.PR != "https://github.com/o/r/pull/1" {
		t.Fatalf("after done: status %s, PR %q", got.Status, got.PR)
	}
	if p := prs(); len(p) != 1 || !strings.HasPrefix(p[0], "main "+tk.Branch+" ") {
		t.Fatalf("PRs = %v, want one against main from %s", p, tk.Branch)
	}
	head := git(t, tk.Worktree, "rev-parse", "HEAD")
	if r := remoteRev(t, origin, tk.Branch); r != head {
		t.Fatalf("origin has %s at %s, want %s", tk.Branch, r, head)
	}
	if out := git(t, origin, "log", "--format=%s", "main.."+tk.Branch); out != "mine" {
		t.Fatalf("main..%s on origin = %q, want only the task's commit", tk.Branch, out)
	}
	if es, _ := a.Store.Train(); len(es) != 0 {
		t.Fatalf("done enqueued into the train: %+v", es)
	}
	if got := git(t, a.Root, "rev-parse", a.Cfg.Integration); got != integ {
		t.Fatalf("done moved the integration branch to %s", got)
	}
	log, _ := os.ReadFile(filepath.Join(a.Root, ".git", FakeGHSubdir, "gh.log"))
	if strings.Contains(string(log), "statuses") || strings.Contains(string(log), "api ") {
		t.Fatalf("done called the forge beyond PRs:\n%s", log)
	}
	if !strings.Contains(string(log), "Closes #12") || !strings.Contains(string(log), "Saddle-Task: t1") {
		t.Fatalf("PR body lacks the issue or trailer:\n%s", log)
	}

	must(t, a.Done(tk.ID, "did my thing"))
	if p := prs(); len(p) != 1 {
		t.Fatalf("done twice opened %d PRs: %v", len(p), p)
	}
	if !hasEvent(t, a, EventPublishNoop, got.PR) {
		t.Fatal("second done at the same head was not a no-op")
	}
}

// #933: with after an unmerged task, the PR's base is that task's branch,
// and it carries only this task's commit.
func TestDoneAfterTaskPRBaseIsItsBranch(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	origin, prs := doneGH(t, a)
	x, err := a.Spawn(SpawnReq{ID: "t1", Title: "dep"})
	must(t, err)
	commitFiles(t, x.Worktree, "dep work", map[string]string{"dep.txt": "dep\n"})
	y, err := a.Spawn(SpawnReq{ID: "t2", Title: "child", After: []string{"t1"}})
	must(t, err)
	commitFiles(t, y.Worktree, "child work", map[string]string{"child.txt": "c\n"})
	must(t, a.Done(y.ID, "child"))
	if p := prs(); len(p) != 1 || !strings.HasPrefix(p[0], x.Branch+" "+y.Branch+" ") {
		t.Fatalf("PRs = %v, want one from %s against %s", p, y.Branch, x.Branch)
	}
	if out := git(t, origin, "log", "--format=%s", x.Branch+".."+y.Branch); out != "child work" {
		t.Fatalf("%s..%s on origin = %q, want only the child's commit", x.Branch, y.Branch, out)
	}
}

// #933: a conflict rebasing onto main at done goes back to the task's own
// agent; it stays running and nothing is pushed.
func TestDoneConflictStaysWithAgent(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	origin, prs := doneGH(t, a)
	tk, err := a.Spawn(SpawnReq{ID: "t1", Title: "mine"})
	must(t, err)
	commitFiles(t, tk.Worktree, "mine", map[string]string{"README.md": "mine\n"})
	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	commitFiles(t, other, "theirs", map[string]string{"README.md": "theirs\n"})
	git(t, other, "push", "-q", "origin", "main")

	err = a.Done(tk.ID, "mine")
	if err == nil || !strings.Contains(err.Error(), "README.md") || !strings.Contains(err.Error(), "saddle sync") {
		t.Fatalf("done = %v, want the conflict sent back", err)
	}
	if got, _ := a.Store.Task(tk.ID); got.Status != store.Running {
		t.Fatalf("status = %s, want running", got.Status)
	}
	if p := prs(); len(p) != 0 || remoteRev(t, origin, tk.Branch) != "" {
		t.Fatalf("a conflicted branch was published: %v", p)
	}
}

// #933: a forge failure leaves the task done with a retry recorded and one
// notice; the retry tick publishes it once the forge answers.
func TestDonePublishRetriesForgeFailure(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	_, prs := doneGH(t, a)
	bin := filepath.Join(a.Root, ".git", FakeGHSubdir)
	good, err := os.ReadFile(filepath.Join(bin, "gh"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\necho 'connection reset by peer' >&2\nexit 1\n"), 0o755))
	tk, err := a.Spawn(SpawnReq{ID: "t1", Title: "mine"})
	must(t, err)
	commitFiles(t, tk.Worktree, "mine", map[string]string{"mine.txt": "m\n"})
	must(t, a.Done(tk.ID, "mine"))
	if got, _ := a.Store.Task(tk.ID); got.Status != store.Done || got.PR != "" {
		t.Fatalf("after a forge failure: status %s, PR %q", got.Status, got.PR)
	}
	at, ok, err := a.Store.RetryAttempt(publishKey(tk.ID))
	must(t, err)
	if !ok {
		t.Fatal("no retry recorded")
	}
	must(t, os.WriteFile(filepath.Join(bin, "gh"), good, 0o755))
	done, err := a.PublishRetryTick(at.NextAt)
	must(t, err)
	if len(done) != 1 || len(prs()) != 1 {
		t.Fatalf("retry published %v; PRs %v", done, prs())
	}
	if got, _ := a.Store.Task(tk.ID); got.Status != store.PROpen {
		t.Fatalf("after retry: status %s", got.Status)
	}
}
