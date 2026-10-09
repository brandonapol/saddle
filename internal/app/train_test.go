package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// trainSetup is setup with tests explicitly off, so land doesn't refuse.
func trainSetup(t *testing.T) *App {
	t.Helper()
	a, _ := setup(t)
	a.Cfg.Test.Cmd = "none"
	return a
}

// originWithGh gives the repo a bare origin holding base, and a fake gh that
// answers gh calls made in the repo or its worktrees (see installGHDispatch).
// It returns the origin's path and a func reading gh's call log.
func originWithGh(t *testing.T, a *App) (string, func() []string) {
	t.Helper()
	origin := t.TempDir()
	git(t, origin, "init", "-q", "--bare", "-b", "main")
	git(t, a.Root, "remote", "add", "origin", origin)
	git(t, a.Root, "push", "-q", "origin", a.Cfg.Base)
	git(t, a.Root, "fetch", "-q", "origin")

	bin := filepath.Join(a.Root, ".git", FakeGHSubdir)
	must(t, os.MkdirAll(bin, 0o755))
	log := filepath.Join(bin, "gh.log")
	// pr view answers from a per-PR file (see setPR), else as an open PR;
	// pr edit --base fails on a closed or merged PR, as GitHub does.
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
f="` + bin + `/view-$(basename "$3")"
case "$1 $2" in
"pr create")
	n=$(grep -c '^pr create' "` + log + `")
	echo "https://github.com/o/r/pull/$n" ;;
"pr view")
	if [ -f "$f" ]; then cat "$f"; else echo '{"state":"OPEN","mergeable":"MERGEABLE","baseRefName":""}'; fi ;;
"pr edit")
	if [ "$4" = "--base" ] && [ -f "$f" ] && grep -q '"CLOSED"\|"MERGED"' "$f"; then
		echo "GraphQL: Cannot change the base branch of a closed pull request. (updatePullRequest)" >&2
		exit 1
	fi ;;
esac
`
	must(t, os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755))
	fakeGHDirs.Store(t.Name(), bin)
	t.Cleanup(func() { fakeGHDirs.Delete(t.Name()) })
	return origin, func() []string {
		b, err := os.ReadFile(log)
		if os.IsNotExist(err) {
			return nil
		}
		must(t, err)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
}

// FakeGHSubdir is where a repo's fake gh lives, under its git common dir.
// Exported for the app_test package's replays.
const FakeGHSubdir = "saddle-test-gh"

// installGHDispatch puts a gh on PATH that runs the fake gh of the repo it is
// called in, and the real gh (if any) elsewhere. Tests that fake gh then need
// no PATH of their own, so they can run in parallel.
func installGHDispatch(bin string) error {
	fallback := `echo "gh: command not found" >&2; exit 127`
	if real, err := exec.LookPath("gh"); err == nil {
		fallback = `exec ` + shellQuote(real) + ` "$@"`
	}
	script := `#!/bin/sh
d=$(git rev-parse --path-format=absolute --git-common-dir 2>/dev/null)/` + FakeGHSubdir + `
if [ -x "$d/gh" ]; then exec "$d/gh" "$@"; fi
` + fallback + "\n"
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755); err != nil {
		return err
	}
	return os.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// fakeGHDirs maps a test's name to its fake gh's directory, so setPR and the
// call log readers can reach it from the test or its subtests.
var fakeGHDirs sync.Map

// fakeGHDir is the fake gh directory of t or its nearest parent, or "".
func fakeGHDir(t *testing.T) string {
	for n := t.Name(); ; {
		if d, ok := fakeGHDirs.Load(n); ok {
			return d.(string)
		}
		i := strings.LastIndex(n, "/")
		if i < 0 {
			return ""
		}
		n = n[:i]
	}
}

// setPR makes the fake gh report the PR at url as state, merged into or
// targeting baseRef.
func setPR(t *testing.T, url, state, baseRef string) {
	t.Helper()
	b := fmt.Sprintf(`{"state":%q,"mergeable":"MERGEABLE","baseRefName":%q}`, state, baseRef)
	dir := fakeGHDir(t)
	if dir == "" {
		t.Fatal("setPR: no fake gh; call originWithGh first")
	}
	must(t, os.WriteFile(filepath.Join(dir, "view-"+filepath.Base(url)), []byte(b), 0o644))
}

// remoteRev is the commit a branch points at in the bare origin, or "".
func remoteRev(t *testing.T, origin, branch string) string {
	t.Helper()
	out, _ := gitx.Run(origin, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return out
}

// landTask spawns a task, commits files on its branch and lands it.
func landTask(t *testing.T, a *App, id, title string, commits ...map[string]string) store.Task {
	t.Helper()
	tk := queueTask(t, a, id, title, commits...)
	rs, err := a.Land()
	must(t, err)
	for _, r := range rs {
		if r.State != store.TrainOK {
			t.Fatalf("land %s: %s %s", r.Task, r.State, r.Note)
		}
	}
	got, err := a.Store.Task(tk.ID)
	must(t, err)
	return got
}

// queueTask spawns a task, commits files on its branch and calls done, so it
// waits in the train.
func queueTask(t *testing.T, a *App, id, title string, commits ...map[string]string) store.Task {
	t.Helper()
	tk, err := a.Spawn(SpawnReq{ID: id, Title: title})
	must(t, err)
	for i, files := range commits {
		for rel, body := range files {
			write(t, tk.Worktree, rel, body)
		}
		msg := title
		if i > 0 {
			msg += " part " + string(rune('1'+i))
		}
		commitAll(t, tk.Worktree, msg)
	}
	must(t, a.Done(tk.ID, title+" summary"))
	return tk
}

func TestLandRefusesWithoutTestCmd(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	tk, err := a.Spawn(SpawnReq{Title: "one"})
	must(t, err)
	write(t, tk.Worktree, "one.txt", "one\n")
	commitAll(t, tk.Worktree, "one")
	must(t, a.Done(tk.ID, "one"))
	before := git(t, a.Root, "rev-parse", a.Cfg.Integration)

	a.Cfg.Test.Cmd = ""
	_, err = a.Land()
	if err == nil || !strings.Contains(err.Error(), `set [test] cmd in .saddle/config.toml, or cmd = "none" to land untested`) {
		t.Fatalf("land without a test cmd: err = %v", err)
	}
	es, err := a.Store.Train()
	must(t, err)
	if len(es) != 1 || es[0].State != store.Queued || es[0].Attempts != 0 {
		t.Fatalf("queue changed: %+v", es)
	}
	if got := git(t, a.Root, "rev-parse", a.Cfg.Integration); got != before {
		t.Fatalf("integration moved to %s", got)
	}

	a.Cfg.Test.Cmd = "none"
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TrainOK {
		t.Fatalf("land with cmd = none: %+v", rs)
	}
}

func TestDetectTestCmd(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	write(t, a.Root, "go.mod", "module x\n")
	write(t, a.Root, "Makefile", "GO := go\n\ncheck: vet test\n\tgo vet ./...\n")
	a.Cfg.Test.Cmd = ""
	must(t, a.Init())
	cfg, err := config.Load(a.Root)
	must(t, err)
	if cfg.Test.Cmd != "make check" {
		t.Fatalf("detected test cmd = %q, want make check", cfg.Test.Cmd)
	}

	cases := map[string]string{
		"go.mod":       "go test ./...",
		"package.json": "npm test",
		"Cargo.toml":   "cargo test",
	}
	for file, want := range cases {
		dir := t.TempDir()
		write(t, dir, file, "\n")
		write(t, dir, "Makefile", "check := yes\nbuild:\n")
		if got := DetectTestCmd(dir); got != want {
			t.Errorf("DetectTestCmd with %s = %q, want %q", file, got, want)
		}
	}
	if got := DetectTestCmd(t.TempDir()); got != "" {
		t.Errorf("DetectTestCmd on an empty repo = %q", got)
	}

	// #163: a package.json check script is the fuller gate, so it wins over npm test.
	dir := t.TempDir()
	write(t, dir, "package.json", `{"scripts": {"test": "vitest", "check": "tsc && vitest"}}`)
	if got := DetectTestCmd(dir); got != "npm run check" {
		t.Errorf("DetectTestCmd with a check script = %q, want npm run check", got)
	}
}

func TestPRsPushesLandedSHAs(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Train.Output = "single" // pins the one linear stack this test was written for (#52)
	origin, ghLog := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	t2 := landTask(t, a, "t2", "two", map[string]string{"two.txt": "two\n"})
	landed := map[string]string{t1.ID: git(t, a.Root, "rev-parse", a.Cfg.Integration+"~1"), t2.ID: git(t, a.Root, "rev-parse", a.Cfg.Integration)}

	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	for _, tk := range []store.Task{t1, t2} {
		if got := remoteRev(t, origin, tk.Branch); got != landed[tk.ID] {
			t.Fatalf("%s: remote %s = %q, landed %s", tk.ID, tk.Branch, got, landed[tk.ID])
		}
	}
	if len(ghLog()) == 0 {
		t.Fatal("gh was not called")
	}
}

func TestPRsRefusesDriftedBranch(t *testing.T) {
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landed := git(t, a.Root, "rev-parse", t1.Branch)

	// Something rewrites the landed branch after the train recorded it. It
	// moves as its owner, so the ref guard lets it through.
	t.Setenv("SADDLE_TASK", t1.ID)
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, a.Root, "worktree", "add", "-q", wt, t1.Branch)
	git(t, wt, "commit", "-q", "--amend", "-m", "one, amended")
	drifted := git(t, a.Root, "rev-parse", t1.Branch)

	_, err := a.PRs()
	want := t1.ID + ": landed " + landed[:12] + ", branch " + drifted[:12]
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("PRs on a drifted branch: err = %v, want %q", err, want)
	}
	if got := remoteRev(t, origin, t1.Branch); got != "" {
		t.Fatalf("drifted branch was pushed: %s", got)
	}
	if l := ghLog(); len(l) > 0 {
		t.Fatalf("gh called: %v", l)
	}
}

// Rebuilds the PR #69 incident: t1 and t2 were rewritten onto a new main, but
// t4 still sits on their old lineage plus a merge commit.
func TestPRsRefusesForkedStack(t *testing.T) {
	a := trainSetup(t)
	a.Cfg.Train.Output = "single" // pins the one linear stack this test was written for (#52)
	origin, ghLog := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "usage", map[string]string{"usage.txt": "usage\n"})
	t2 := landTask(t, a, "t2", "planner", map[string]string{"planner.txt": "planner\n"})
	t4 := landTask(t, a, "t4", "ciwatch", map[string]string{"ciwatch.txt": "ciwatch\n"})
	c1, c2, c4 := git(t, a.Root, "rev-parse", t1.Branch), git(t, a.Root, "rev-parse", t2.Branch), git(t, a.Root, "rev-parse", t4.Branch)

	// main moves; t1 and t2 are rebased onto it by hand and recorded as landed.
	write(t, a.Root, "main.txt", "main\n")
	commitAll(t, a.Root, "main moves")
	m := git(t, a.Root, "rev-parse", "HEAD")
	git(t, a.Root, "push", "-q", "origin", "main")
	git(t, a.Root, "fetch", "-q", "origin")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, a.Root, "worktree", "add", "-q", "--detach", wt, m)
	git(t, wt, "cherry-pick", c1)
	n1 := git(t, wt, "rev-parse", "HEAD")
	git(t, wt, "cherry-pick", c2)
	n2 := git(t, wt, "rev-parse", "HEAD")

	// The old lineage gets a side commit merged in, and t4 is rebuilt on top.
	git(t, wt, "checkout", "-q", "--detach", m+"~1")
	write(t, wt, "watcher.txt", "watcher\n")
	commitAll(t, wt, "git watcher")
	x := git(t, wt, "rev-parse", "HEAD")
	git(t, wt, "checkout", "-q", "--detach", c2)
	git(t, wt, "merge", "-q", "--no-ff", "-m", "merge main", x)
	g := git(t, wt, "rev-parse", "HEAD")
	git(t, wt, "cherry-pick", c4)
	n4 := git(t, wt, "rev-parse", "HEAD")

	for _, r := range []struct {
		tk       store.Task
		from, to string
	}{{t1, m, n1}, {t2, n1, n2}, {t4, g, n4}} {
		t.Setenv("SADDLE_TASK", r.tk.ID) // each branch moves as its owner
		git(t, a.Root, "update-ref", "refs/heads/"+r.tk.Branch, r.to)
		must(t, a.Store.SetTrain(r.tk.ID, store.TrainOK, r.from+".."+r.to, false))
	}

	_, err := a.PRs()
	if err == nil {
		t.Fatal("PRs accepted a forked stack")
	}
	msg := err.Error()
	for _, want := range []string{"t4", "contains 3 commits from other tasks, 1 merge commit", "restack"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not mention %q", msg, want)
		}
	}
	if strings.Contains(msg, "t1:") || strings.Contains(msg, "t2:") {
		t.Fatalf("error blames healthy layers: %s", msg)
	}
	// Only the broken layer is frozen (#119.4): t1 and t2 below it publish.
	for tk, want := range map[store.Task]string{t1: n1, t2: n2, t4: ""} {
		if got := remoteRev(t, origin, tk.Branch); got != want {
			t.Fatalf("remote %s = %q, want %q", tk.Branch, got, want)
		}
	}
	for _, c := range ghLog() {
		if strings.Contains(c, t4.Branch) {
			t.Fatalf("gh touched t4: %s", c)
		}
	}
}

// While the stack is flagged at risk from its bottom, prs pushes and opens
// nothing, and land holds work that touches the broken layer.
func TestFlaggedStackFreezesPRsAndLand(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	origin, ghLog := originWithGh(t, a)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	tk, err := a.Spawn(SpawnReq{ID: "t2", Title: "two"})
	must(t, err)
	write(t, tk.Worktree, "one.txt", "one\ntwo\n")
	commitAll(t, tk.Worktree, "two")
	must(t, a.Done(tk.ID, "two"))
	before := git(t, a.Root, "rev-parse", a.Cfg.Integration)

	must(t, a.SetFlag(StackFlag{Task: t1.ID, Cause: "GitHub reports its PR conflicts with its base"}))
	for name, call := range map[string]func() error{
		"PRs":  func() error { _, err := a.PRs(); return err },
		"Land": func() error { _, err := a.Land(); return err },
	} {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "restack") || !strings.Contains(err.Error(), t1.ID) {
			t.Fatalf("%s while flagged: err = %v", name, err)
		}
	}
	if got := remoteRev(t, origin, t1.Branch); got != "" {
		t.Fatal("pushed while flagged")
	}
	if l := ghLog(); len(l) > 0 {
		t.Fatalf("gh called while flagged: %v", l)
	}
	if got := git(t, a.Root, "rev-parse", a.Cfg.Integration); got != before {
		t.Fatal("landed while flagged")
	}

	must(t, a.ClearFlag())
	if _, err := a.PRs(); err != nil {
		t.Fatalf("PRs after the flag cleared: %v", err)
	}
}

// #184: a gate that keeps failing on the environment isn't the branch's
// fault: the task stays queued, is not returned to its producer, isn't
// charged an attempt, and the orchestrator hears what to free.
func TestLandEnvironmentFailureKeepsTaskQueued(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	a.Cfg.Test.Cmd = "echo 'write /tmp/x: disk quota exceeded'; exit 1"
	tk := queueTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	before := git(t, a.Root, "rev-parse", a.Cfg.Integration)
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TrainError || !strings.Contains(rs[0].Note, "environment") {
		t.Fatalf("land = %+v, want an environment error", rs)
	}
	if git(t, a.Root, "rev-parse", a.Cfg.Integration) != before {
		t.Fatal("integration moved on a red gate")
	}
	if got, _ := a.Store.Task(tk.ID); got.Status == store.Conflict || got.Status == store.NeedsYou {
		t.Fatalf("task status = %s, want it left alone", got.Status)
	}
	es, err := a.Store.Train()
	must(t, err)
	for _, e := range es {
		if e.Task == tk.ID && (e.State != store.Queued || e.Attempts != 0) {
			t.Fatalf("train row = %+v, want queued with no attempts", e)
		}
	}
	if ns, _ := a.Store.TakeNotices(tk.ID, false); len(ns) != 0 {
		t.Fatalf("producer told: %+v", ns)
	}
	ns, err := a.Store.TakeNotices(OrchestratorID, false)
	must(t, err)
	if !slicesContainsText(ns, "disk quota exceeded") {
		t.Fatalf("orchestrator notices = %+v, want the environment problem", ns)
	}
}

func slicesContainsText(ns []store.Notice, s string) bool {
	for _, n := range ns {
		if strings.Contains(n.Text, s) {
			return true
		}
	}
	return false
}

// #269: a hung test gate times out, fails the branch with its output tail
// and how long it ran, and lets go of train.lock for the next land.
func TestLandTimesOutAHungGate(t *testing.T) {
	a := trainSetup(t)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	tk := queueTask(t, a, "t1", "hang", map[string]string{"x.go": "x\n"})
	setGateTimeout(t, "300ms")
	a.Cfg.Test.Cmd = "echo started; sleep 600"
	rs, err := a.Land()
	must(t, err)
	if len(rs) != 1 || rs[0].State != store.TestFailed {
		t.Fatalf("results = %+v", rs)
	}
	ns, err := a.Store.TakeNotices(tk.ID, false)
	must(t, err)
	if len(ns) == 0 || !strings.Contains(ns[len(ns)-1].Text, "started") || !strings.Contains(ns[len(ns)-1].Text, "gate timed out after 300ms") {
		t.Fatalf("notices = %+v", ns)
	}
	unlock, ok, err := a.TryLockTrain()
	must(t, err)
	if !ok {
		t.Fatal("train.lock still held after the gate timed out")
	}
	unlock()
}

// holdTrainLock takes train.lock on a separate open file, as another
// process would, with holder pid since the given time. It returns the file.
func holdTrainLock(t *testing.T, a *App, pid int, since time.Time) *os.File {
	t.Helper()
	f, err := os.OpenFile(a.stateDir("train.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	must(t, err)
	t.Cleanup(func() { f.Close() })
	must(t, syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	_, err = fmt.Fprintf(f, "%d %d\n", pid, since.UnixNano())
	must(t, err)
	return f
}

func lockStolen(t *testing.T, a *App) bool {
	t.Helper()
	evs, err := a.Store.Events(100)
	must(t, err)
	for _, e := range evs {
		if e.Kind == EventTrainLockStolen {
			return true
		}
	}
	return false
}

// #269 watchdog: a train.lock whose holder is dead (its lock kept by a
// leaked descriptor) is stolen, with an event.
func TestLockTrainStealsFromADeadHolder(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	c := exec.Command("true")
	must(t, c.Run())
	holdTrainLock(t, a, c.Process.Pid, time.Now())
	done := make(chan error, 1)
	go func() {
		unlock, err := a.lockTrain()
		if err == nil {
			unlock()
		}
		done <- err
	}()
	select {
	case err := <-done:
		must(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("lockTrain waited on a dead holder")
	}
	if !lockStolen(t, a) {
		t.Fatal("no event for the stolen lock")
	}
}

// #269 watchdog: a live holder that has had train.lock for over twice
// [train] gate_timeout is stuck past any gate; it is stopped and the lock
// stolen.
func TestLockTrainStealsFromAnOverdueHolder(t *testing.T) {
	a := trainSetup(t)
	setGateTimeout(t, "300ms")
	holder := exec.Command("sleep", "600")
	must(t, holder.Start())
	exited := make(chan struct{})
	go func() { _ = holder.Wait(); close(exited) }()
	t.Cleanup(func() { _ = holder.Process.Kill() })
	holdTrainLock(t, a, holder.Process.Pid, time.Now())
	unlock, err := a.lockTrain()
	must(t, err)
	unlock()
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the overdue holder was left running")
	}
	if !lockStolen(t, a) {
		t.Fatal("no event for the stolen lock")
	}
}

// A holder within its time keeps the lock.
func TestLockTrainWaitsOnALiveHolder(t *testing.T) {
	t.Parallel()
	a := trainSetup(t)
	f := holdTrainLock(t, a, os.Getpid(), time.Now())
	_, ok, err := a.TryLockTrain()
	must(t, err)
	if ok {
		t.Fatal("took a lock a live holder has")
	}
	must(t, syscall.Flock(int(f.Fd()), syscall.LOCK_UN))
	unlock, ok, err := a.TryLockTrain()
	must(t, err)
	if !ok {
		t.Fatal("lock free but not taken")
	}
	unlock()
	if lockStolen(t, a) {
		t.Fatal("stole a live holder's lock")
	}
}

// #268: landing N disjoint queued tasks doesn't auto-rebase the ones still
// waiting after each landing (N(N-1)/2 extra rebases); the train rebases each
// once, on its turn. An idle worker still gets rebased, and a queued task the
// run doesn't land hears about the landings once, not once per landing.
func TestLandDoesNotAutoRebaseQueuedTasks(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	idle, err := a.Spawn(SpawnReq{Title: "idle"})
	must(t, err)
	write(t, idle.Worktree, "idle.txt", "idle\n")
	commitAll(t, idle.Worktree, "idle")
	const n = 5
	ts := queued(t, a, n)
	last := ts[n-1]
	must(t, a.Hold(last.ID, "later"))

	rs, err := a.Land()
	must(t, err)
	if len(rs) != n-1 {
		t.Fatalf("land = %+v", rs)
	}
	es, err := a.Store.Events(-1)
	must(t, err)
	rebases := map[string]int{}
	for _, e := range es {
		if e.Kind == "auto_rebase" {
			rebases[e.Task]++
		}
	}
	for _, tk := range ts[:n-1] {
		if rebases[tk.ID] != 0 {
			t.Errorf("queued %s auto-rebased %d times while it waited", tk.ID, rebases[tk.ID])
		}
	}
	if rebases[idle.ID] == 0 {
		t.Error("idle worker was not auto-rebased")
	}
	ns, err := a.Store.TakeNotices(last.ID, false)
	must(t, err)
	if len(ns) != 1 || !strings.Contains(ns[0].Text, ts[0].ID) || !strings.Contains(ns[0].Text, ts[n-2].ID) {
		t.Fatalf("held task notices = %+v", ns)
	}
}
