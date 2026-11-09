package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/forge"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

// saddle flatten (#937) against the merge queue's in-memory forge over a
// real bare origin. Every forge write is logged in calls.

type flatForge struct {
	*qForge
	calls  []string
	bodies map[string]string
	titles map[string]string
}

func (f *flatForge) SetPRBase(ctx context.Context, ref, base string) error {
	f.calls = append(f.calls, "base "+ref+" "+base)
	return f.qForge.SetPRBase(ctx, ref, base)
}

func (f *flatForge) SetPRBody(_ context.Context, ref, body string) error {
	f.calls = append(f.calls, "body "+ref)
	f.bodies[ref] = body
	return nil
}

func (f *flatForge) SetPRTitle(_ context.Context, ref, title string) error {
	f.calls = append(f.calls, "title "+ref+" "+title)
	f.titles[ref] = title
	return nil
}

func (f *flatForge) ClosePR(_ context.Context, ref string) error {
	f.calls = append(f.calls, "close "+ref)
	if p := f.find(ref); p != nil {
		p.state = forge.StateClosed
	}
	return nil
}

func (f *flatForge) AddComment(_ context.Context, ref, body string) error {
	f.calls = append(f.calls, "comment "+ref+" "+body)
	return nil
}

// flatWorld is an App on the old train (legacy_train on) with a bare
// origin and a clone of it to build branches in.
type flatWorld struct {
	a     *App
	f     *flatForge
	clone string
	n     int
}

func flattenSetup(t *testing.T) *flatWorld {
	t.Helper()
	a, qf, _ := queueSetup(t)
	if _, err := a.SetConfig("legacy_train", "true"); err != nil {
		t.Fatal(err)
	}
	a.Cfg.LegacyTrain = true
	clone := t.TempDir()
	git(t, clone, "clone", "-q", qf.origin, ".")
	return &flatWorld{a: a, f: &flatForge{qForge: qf, bodies: map[string]string{}, titles: map[string]string{}}, clone: clone}
}

// commit commits file=body in the clone on the branch checked out there,
// at a fixed date so two copies of a change get two different shas.
func (w *flatWorld) commit(t *testing.T, msg, file, body, date string) string {
	t.Helper()
	write(t, w.clone, file, body)
	git(t, w.clone, "add", "-A")
	env := []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
	gitEnv(t, w.clone, env, "commit", "-qm", msg)
	return git(t, w.clone, "rev-parse", "HEAD")
}

func (w *flatWorld) push(t *testing.T, branch string) {
	t.Helper()
	git(t, w.clone, "push", "-q", "-f", "origin", "HEAD:refs/heads/"+branch)
}

// landed records a task the old train landed, with its open PR against base.
func (w *flatWorld) landed(t *testing.T, id, title, branch, base string) *qPR {
	t.Helper()
	w.n++
	p := &qPR{n: w.n, url: fmt.Sprintf("https://github.com/o/r/pull/%d", w.n), head: branch, base: base, state: forge.StateOpen, checks: "pass"}
	w.f.prs = append(w.f.prs, p)
	must(t, w.a.Store.CreateTask(store.Task{ID: id, Title: title, Role: store.RoleWorker, Branch: branch, Status: store.Landed,
		PR: p.url, Summary: title + " summary"}))
	must(t, w.a.Store.Enqueue(id))
	must(t, w.a.Store.SetTrain(id, store.TrainOK, "", false))
	return p
}

func (w *flatWorld) originLog(t *testing.T, rng string) string {
	t.Helper()
	return git(t, w.f.origin, "log", "--format=%s", "--reverse", rng)
}

// stack3 builds a 3-deep stack t1 ← t2 ← t3 in which the middle PR lacks
// its base: t2 was cut from an older copy of t1's commit, and also carries
// a copy of a commit main has since.
func stack3(t *testing.T) *flatWorld {
	t.Helper()
	w := flattenSetup(t)
	git(t, w.clone, "checkout", "-q", "-b", "b1", "main")
	w.commit(t, "t1 work", "t1.txt", "t1\n", "2026-01-01T00:00:00")
	w.push(t, "saddle/t1")
	git(t, w.clone, "checkout", "-q", "-b", "b2", "main")
	w.commit(t, "t1 work", "t1.txt", "t1\n", "2026-01-02T00:00:00") // the old copy
	w.commit(t, "news", "news.txt", "news\n", "2026-01-02T00:00:01")
	w.commit(t, "t2 work", "t2.txt", "t2\n", "2026-01-02T00:00:02")
	w.push(t, "saddle/t2")
	git(t, w.clone, "checkout", "-q", "-b", "b3")
	w.commit(t, "t3 work", "t3.txt", "t3\n", "2026-01-03T00:00:00")
	w.push(t, "saddle/t3")
	// main squash-merged the news since.
	git(t, w.clone, "checkout", "-q", "main")
	w.commit(t, "news (#9)", "news.txt", "news\n", "2026-01-04T00:00:00")
	w.push(t, "main")
	w.landed(t, "t1", "One", "saddle/t1", "main")
	w.landed(t, "t2", "Two", "saddle/t2", "saddle/t1")
	w.landed(t, "t3", "Three", "saddle/t3", "saddle/t2")
	return w
}

func refs(t *testing.T, dir string) string {
	t.Helper()
	return git(t, dir, "for-each-ref", "--format=%(refname) %(objectname)")
}

// #937: a 3-deep stack whose middle PR lacks its base flattens to three PRs
// on main, each with only its own commits; bottom-up; the old train's state
// is cleared and legacy_train turned off.
func TestFlattenThreeDeepStackOntoMain(t *testing.T) {
	t.Parallel()
	w := stack3(t)
	a := w.a
	res, err := a.Flatten(t.Context(), w.f, false)
	must(t, err)
	if len(res.PRs) != 3 || res.PRs[0].Task != "t1" || res.PRs[1].Task != "t2" || res.PRs[2].Task != "t3" {
		t.Fatalf("PRs = %+v, want t1, t2, t3 bottom-up", res.PRs)
	}
	for _, id := range []string{"t1", "t2", "t3"} {
		if got := w.originLog(t, "main..saddle/"+id); got != id+" work" {
			t.Errorf("main..saddle/%s = %q, want only %q", id, got, id+" work")
		}
		tk, err := a.Store.Task(id)
		must(t, err)
		if tk.Status != store.PROpen || tk.Base != "main" {
			t.Errorf("%s is %s on %q, want pr_open on main", id, tk.Status, tk.Base)
		}
		if b := w.f.bodies[tk.PR]; !strings.Contains(b, "Saddle-Task: "+id) || !strings.Contains(b, tk.Title+" summary") {
			t.Errorf("%s's PR body = %q", id, b)
		}
	}
	for _, p := range w.f.prs {
		if p.base != "main" {
			t.Errorf("PR #%d targets %s, want main", p.n, p.base)
		}
	}
	if got := strings.Join(w.f.baseEdits, ", "); got != "#2 main, #3 main" {
		t.Errorf("retargets = %s, want #2 then #3", got)
	}
	drop := res.PRs[1].Drop
	if len(drop) != 2 || drop[0].Owner != "t1" || drop[1].Owner != "main" {
		t.Errorf("t2 dropped %+v, want t1's old copy and the news on main", drop)
	}
	if tr, _ := a.Store.Train(); len(tr) != 0 {
		t.Errorf("train = %+v, want the flattened tasks' entries gone", tr)
	}
	if !res.LegacyTrainOff || a.Cfg.LegacyTrain {
		t.Error("legacy_train is still on")
	}
	b, err := os.ReadFile(filepath.Join(a.Root, ".saddle", "config.toml"))
	must(t, err)
	if !strings.Contains(string(b), "legacy_train = false") {
		t.Errorf("config.toml doesn't set legacy_train = false:\n%s", b)
	}
	if !strings.Contains(res.Text(), "legacy_train was on") {
		t.Errorf("the output doesn't say legacy_train was turned off:\n%s", res.Text())
	}
}

// #937: --dry-run prints each PR's kept and dropped commits and plan, and
// changes no ref, PR or task.
func TestFlattenDryRunChangesNothing(t *testing.T) {
	t.Parallel()
	w := stack3(t)
	a := w.a
	beforeOrigin, beforeRoot := refs(t, w.f.origin), refs(t, a.Root)
	beforeTasks, _ := a.Store.Tasks()
	beforeTrain, _ := a.Store.Train()
	res, err := a.Flatten(t.Context(), w.f, true)
	must(t, err)
	if len(w.f.calls) != 0 {
		t.Errorf("the dry run wrote to the forge: %v", w.f.calls)
	}
	if got := refs(t, w.f.origin); got != beforeOrigin {
		t.Errorf("origin's refs changed:\n%s\n→\n%s", beforeOrigin, got)
	}
	if got := refs(t, a.Root); got != beforeRoot {
		// Only the remote-tracking refs a fetch updates may differ.
		for _, l := range strings.Split(got, "\n") {
			if !strings.HasPrefix(l, "refs/remotes/") && !strings.Contains(beforeRoot, l) {
				t.Errorf("a local ref changed: %s", l)
			}
		}
	}
	afterTasks, _ := a.Store.Tasks()
	for i := range beforeTasks {
		if beforeTasks[i].Status != afterTasks[i].Status || beforeTasks[i].Base != afterTasks[i].Base {
			t.Errorf("%s changed: %s/%q → %s/%q", beforeTasks[i].ID, beforeTasks[i].Status, beforeTasks[i].Base, afterTasks[i].Status, afterTasks[i].Base)
		}
	}
	if afterTrain, _ := a.Store.Train(); len(afterTrain) != len(beforeTrain) {
		t.Error("the dry run changed the train")
	}
	if !a.Cfg.LegacyTrain || res.LegacyTrainOff {
		t.Error("the dry run turned legacy_train off")
	}
	if _, err := os.Stat(a.flattenScratch()); err == nil {
		t.Error("the scratch worktree was left behind")
	}
	out := res.Text()
	for _, want := range []string{"nothing was changed", "#2 t2", "keep", "t2 work", "drop", "t1's (#1), the PR below it", "on main as", "base: saddle/t1 → main", "plan: replay"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dry run's output lacks %q:\n%s", want, out)
		}
	}
}

// #937: a replay that conflicts leaves the PR exactly as it was and
// reopens its task to fix in place.
func TestFlattenConflictLeavesPRAndReopensTask(t *testing.T) {
	t.Parallel()
	w := flattenSetup(t)
	a := w.a
	git(t, w.clone, "checkout", "-q", "-b", "b1", "main")
	w.commit(t, "t1 work", "README.md", "t1\n", "2026-01-01T00:00:00")
	w.push(t, "saddle/t1")
	git(t, w.clone, "checkout", "-q", "main")
	w.commit(t, "main work", "README.md", "main\n", "2026-01-02T00:00:00")
	w.push(t, "main")
	w.landed(t, "t1", "One", "saddle/t1", "main")
	head := w.f.rev("saddle/t1")
	res, err := a.Flatten(t.Context(), w.f, false)
	must(t, err)
	if len(res.PRs) != 1 || res.PRs[0].Outcome != FlattenFix {
		t.Fatalf("PRs = %+v, want t1 fixing", res.PRs)
	}
	if len(w.f.calls) != 0 {
		t.Errorf("the PR was touched: %v", w.f.calls)
	}
	if w.f.rev("saddle/t1") != head {
		t.Error("the branch was pushed")
	}
	tk, err := a.Store.Task("t1")
	must(t, err)
	if tk.Status != store.Fixing || !strings.HasPrefix(tk.FixReason, "conflict: README.md") || tk.Base != "main" {
		t.Errorf("t1 = %s %q base %q, want fixing with conflict: README.md on main", tk.Status, tk.FixReason, tk.Base)
	}
}

// #937: a PR whose commits are all on main is closed with a comment naming
// the main commit; its task is merged.
func TestFlattenClosesPRAlreadyOnMain(t *testing.T) {
	t.Parallel()
	w := flattenSetup(t)
	a := w.a
	git(t, w.clone, "checkout", "-q", "-b", "b1", "main")
	w.commit(t, "t1 work", "t1.txt", "t1\n", "2026-01-01T00:00:00")
	w.push(t, "saddle/t1")
	git(t, w.clone, "checkout", "-q", "main")
	sq := w.commit(t, "One (#1)", "t1.txt", "t1\n", "2026-01-02T00:00:00")
	w.push(t, "main")
	w.landed(t, "t1", "One", "saddle/t1", "main")
	res, err := a.Flatten(t.Context(), w.f, false)
	must(t, err)
	if res.PRs[0].Outcome != FlattenClose {
		t.Fatalf("outcome = %s (%s), want close", res.PRs[0].Outcome, res.PRs[0].Reason)
	}
	comment := ""
	for _, c := range w.f.calls {
		if strings.HasPrefix(c, "comment ") {
			comment = c
		}
	}
	if !strings.Contains(comment, short(sq)) {
		t.Errorf("the comment %q doesn't name main's commit %s", comment, short(sq))
	}
	if w.f.prs[0].state != forge.StateClosed {
		t.Error("the PR is still open")
	}
	if s := taskStatus(t, a, "t1"); s != store.Merged {
		t.Errorf("t1 is %s, want merged", s)
	}
}

// #937: a commit without a trailer that two PRs side by side both carry
// can't be attributed: the dry run says so, and the real run leaves both
// PRs alone with the reason.
func TestFlattenSharedCommitIsUnattributable(t *testing.T) {
	t.Parallel()
	w := flattenSetup(t)
	a := w.a
	for i, id := range []string{"t1", "t2"} {
		git(t, w.clone, "checkout", "-q", "-b", "b"+id, "main")
		w.commit(t, "shared", "shared.txt", "s\n", fmt.Sprintf("2026-01-0%dT00:00:00", i+1))
		w.commit(t, id+" work", id+".txt", id+"\n", fmt.Sprintf("2026-01-0%dT00:00:01", i+1))
		w.push(t, "saddle/"+id)
		w.landed(t, id, strings.ToUpper(id), "saddle/"+id, "main")
	}
	dry, err := a.Flatten(t.Context(), w.f, true)
	must(t, err)
	for _, p := range dry.PRs {
		if len(p.Unattributable) != 1 || p.Unattributable[0].Subject != "shared" || p.Action != FlattenLeave {
			t.Errorf("%s: unattributable %+v, action %s", p.Task, p.Unattributable, p.Action)
		}
	}
	if out := dry.Text(); !strings.Contains(out, "unattributable: no trailer, and also in #2 (t2)") {
		t.Errorf("the dry run doesn't name the shared commit:\n%s", out)
	}
	res, err := a.Flatten(t.Context(), w.f, false)
	must(t, err)
	if len(w.f.calls) != 0 {
		t.Errorf("a PR was touched: %v", w.f.calls)
	}
	for _, p := range res.PRs {
		if p.Outcome != FlattenLeave || !strings.Contains(p.Reason, "can't be attributed") {
			t.Errorf("%s: %s (%s)", p.Task, p.Outcome, p.Reason)
		}
		if s := taskStatus(t, a, p.Task); s != store.Landed {
			t.Errorf("%s is %s, want left landed", p.Task, s)
		}
	}
}

// #937: a trailer decides over ranges, and a repair's PR keeps the
// repair's task, drops "Repair tN " from its title, and its original is
// killed as superseded by flatten.
func TestFlattenRepairKeepsRepairTaskAndDropsPrefix(t *testing.T) {
	t.Parallel()
	w := flattenSetup(t)
	a := w.a
	must(t, a.Store.CreateTask(store.Task{ID: "t5", Title: "Town map", Role: store.RoleWorker, Branch: "saddle/t5", Status: store.Landed}))
	git(t, w.clone, "checkout", "-q", "-b", "b6", "main")
	w.commit(t, "town map\n\nSaddle-Task: t5", "map.md", "map\n", "2026-01-01T00:00:00")
	w.commit(t, "repair it", "map.md", "map 2\n", "2026-01-01T00:00:01")
	w.push(t, "saddle/t6")
	w.landed(t, "t6", "Repair t5 Town map", "saddle/t6", "main")
	res, err := a.Flatten(t.Context(), w.f, false)
	must(t, err)
	p := res.PRs[0]
	if p.Outcome != FlattenReplay || len(p.Keep) != 2 || p.Supersedes != "t5" {
		t.Fatalf("PR = %+v", p)
	}
	if w.f.titles[p.PR] != "Town map" {
		t.Errorf("title = %q, want %q", w.f.titles[p.PR], "Town map")
	}
	t6, _ := a.Store.Task("t6")
	t5, _ := a.Store.Task("t5")
	if t6.Title != "Town map" || t6.Status != store.PROpen {
		t.Errorf("t6 = %q %s", t6.Title, t6.Status)
	}
	if t5.Status != store.Killed || t5.KilledBy != KilledBySupersededByFlatten {
		t.Errorf("t5 = %s killed_by %q, want killed superseded_by_flatten", t5.Status, t5.KilledBy)
	}
}

// #937: after a flatten conflict, a task with no live agent has its local
// branch moved, in its clean worktree, to main plus its own commits up to
// the conflicting one; the rest are named in order in its fix reason, the
// PR keeps its old head, and once the agent applies the rest its done
// passes the own-commits check.
func TestFlattenConflictMovesBranchToOwnWorkOnMain(t *testing.T) {
	t.Parallel()
	w := flattenSetup(t)
	a := w.a
	// t0 below, t1 stacked on it: t1 carries t0's commit.
	git(t, w.clone, "checkout", "-q", "-b", "b0", "main")
	w.commit(t, "t0 work", "t0.txt", "t0\n", "2026-01-01T00:00:00")
	w.push(t, "saddle/t0")
	git(t, w.clone, "checkout", "-q", "-b", "b1")
	w.commit(t, "t1 a", "t1a.txt", "a\n", "2026-01-01T00:00:01")
	pick1 := w.commit(t, "t1 readme", "README.md", "t1\n", "2026-01-01T00:00:02")
	pick2 := w.commit(t, "t1 c", "t1c.txt", "c\n", "2026-01-01T00:00:03")
	w.push(t, "saddle/t1")
	git(t, w.clone, "checkout", "-q", "main")
	w.commit(t, "main readme", "README.md", "main\n", "2026-01-02T00:00:00")
	w.push(t, "main")
	w.landed(t, "t0", "Zero", "saddle/t0", "main")
	w.landed(t, "t1", "One", "saddle/t1", "saddle/t0")
	// Both tasks' old local branches, t1's checked out in its worktree.
	git(t, a.Root, "fetch", "-q", "origin", "saddle/t0:saddle/t0", "saddle/t1:saddle/t1")
	wt := filepath.Join(t.TempDir(), "t1")
	git(t, a.Root, "worktree", "add", "-q", wt, "saddle/t1")
	must(t, a.Store.SetField("t1", "worktree", wt))
	oldHead := w.f.rev("saddle/t1")

	res, err := a.Flatten(t.Context(), w.f, false)
	must(t, err)
	if len(res.PRs) != 2 || res.PRs[1].Task != "t1" || res.PRs[1].Outcome != FlattenFix {
		t.Fatalf("PRs = %+v, want t1 fixing", res.PRs)
	}
	if w.f.rev("saddle/t1") != oldHead {
		t.Error("the PR's head moved; done force-pushes it, not flatten")
	}
	if got := git(t, a.Root, "log", "--format=%s", "--reverse", "origin/main..saddle/t1"); got != "t1 a" {
		t.Errorf("origin/main..saddle/t1 = %q, want only t1's commit before the conflict", got)
	}
	if st := git(t, wt, "status", "--porcelain"); st != "" {
		t.Errorf("the worktree isn't clean: %q", st)
	}
	if _, err := gitx.Run(wt, "rev-parse", "-q", "--verify", "CHERRY_PICK_HEAD"); err == nil {
		t.Error("a cherry-pick was left in the worktree")
	}
	tk, err := a.Store.Task("t1")
	must(t, err)
	want := "conflict: README.md; cherry-pick in order: " + short(pick1) + " t1 readme, " + short(pick2) + " t1 c"
	if tk.FixReason != want {
		t.Errorf("fix_reason = %q, want %q", tk.FixReason, want)
	}
	if note := fixNote(tk, 2); !strings.Contains(note, "your branch now holds only your own work on main. Cherry-pick these in order, resolve the conflict in README.md, then call done.") {
		t.Errorf("fix note = %q", note)
	}
	// The fix pass starting its agent must not reset it to the PR's old
	// stacked head.
	a.adoptRemoteHead(tk, false)
	if got := git(t, a.Root, "log", "--format=%s", "origin/main..saddle/t1"); got != "t1 a" {
		t.Errorf("after adoptRemoteHead origin/main..saddle/t1 = %q", got)
	}
	// The agent applies the rest.
	if _, err := gitx.Run(wt, "cherry-pick", pick1); err == nil {
		t.Fatal("the README pick should conflict")
	}
	write(t, wt, "README.md", "main\nt1\n")
	git(t, wt, "add", "README.md")
	gitEnv(t, wt, []string{"GIT_EDITOR=true"}, "cherry-pick", "--continue")
	git(t, wt, "cherry-pick", pick2)
	tk, _ = a.Store.Task("t1")
	foreign, err := a.foreignCommits(tk, "origin/main")
	must(t, err)
	if len(foreign) != 0 {
		t.Errorf("done's own-commits check refuses: %v", foreign)
	}
}

// #937: a dirty worktree is not moved; the fix reason says so.
func TestFlattenConflictLeavesDirtyWorktree(t *testing.T) {
	t.Parallel()
	w := flattenSetup(t)
	a := w.a
	git(t, w.clone, "checkout", "-q", "-b", "b1", "main")
	w.commit(t, "t1 work", "README.md", "t1\n", "2026-01-01T00:00:00")
	w.push(t, "saddle/t1")
	git(t, w.clone, "checkout", "-q", "main")
	w.commit(t, "main work", "README.md", "main\n", "2026-01-02T00:00:00")
	w.push(t, "main")
	w.landed(t, "t1", "One", "saddle/t1", "main")
	git(t, a.Root, "fetch", "-q", "origin", "saddle/t1:saddle/t1")
	wt := filepath.Join(t.TempDir(), "t1")
	git(t, a.Root, "worktree", "add", "-q", wt, "saddle/t1")
	must(t, a.Store.SetField("t1", "worktree", wt))
	write(t, wt, "scratch.txt", "wip\n")
	before := git(t, wt, "rev-parse", "HEAD")
	_, err := a.Flatten(t.Context(), w.f, false)
	must(t, err)
	if h := git(t, wt, "rev-parse", "HEAD"); h != before {
		t.Error("a dirty worktree was moved")
	}
	tk, _ := a.Store.Task("t1")
	if !strings.Contains(tk.FixReason, "its branch was not moved: its worktree has uncommitted changes") {
		t.Errorf("fix_reason = %q", tk.FixReason)
	}
}
