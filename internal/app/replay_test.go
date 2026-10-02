package app_test

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/refguard"
	"github.com/brandonapol/saddle/internal/sentinel"
	"github.com/brandonapol/saddle/internal/store"
)

// replayTmux is a tmux with every window alive until killed.
type replayTmux struct {
	windows map[string]string
	n       int
}

func (f *replayTmux) HasSession() bool { return f.n > 0 }
func (f *replayTmux) NewSession(name, dir, cmd string) (string, error) {
	return f.NewWindow(name, dir, cmd)
}
func (f *replayTmux) NewWindow(name, _, _ string) (string, error) {
	f.n++
	id := fmt.Sprintf("@%d", f.n)
	f.windows[id] = name
	return id, nil
}
func (f *replayTmux) WindowName(id string) (string, error) { return f.windows[id], nil }
func (f *replayTmux) KillWindow(id string) error           { delete(f.windows, id); return nil }
func (f *replayTmux) Alive(id string) bool                 { _, ok := f.windows[id]; return ok }
func (f *replayTmux) SendText(string, string) error        { return nil }
func (f *replayTmux) SendKeys(string, ...string) error     { return nil }
func (f *replayTmux) Capture(string, int) (string, error)  { return "", nil }
func (f *replayTmux) KillSession() error                   { f.windows = map[string]string{}; return nil }

// replay drives git as one actor, the way that actor's process would: the ref
// guard reads SADDLE_TASK, and SADDLE_TRAIN is never set outside the train.
type replay struct {
	t      *testing.T
	a      *app.App
	origin string
	ghDir  string
}

func (r *replay) run(dir, actor string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "SADDLE_TASK="+actor, "SADDLE_TRAIN=")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (r *replay) git(dir, actor string, args ...string) string {
	r.t.Helper()
	out, err := r.run(dir, actor, args...)
	if err != nil {
		r.t.Fatalf("%s: git %s: %v: %s", actor, strings.Join(args, " "), err, out)
	}
	return out
}

func (r *replay) write(dir, rel, body string) {
	r.t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		r.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *replay) commit(dir, actor, msg string, files map[string]string) {
	r.t.Helper()
	for rel, body := range files {
		r.write(dir, rel, body)
	}
	r.git(dir, actor, "add", "-A")
	r.git(dir, actor, "commit", "-qm", msg)
}

// ghLog is every call the fake gh got, in order.
func (r *replay) ghLog() []string {
	r.t.Helper()
	b, err := os.ReadFile(filepath.Join(r.ghDir, "gh.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		r.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// setPR makes the fake gh report state for the PR at url.
func (r *replay) setPR(url, state, mergeable string) {
	r.t.Helper()
	r.setPRBase(url, state, mergeable, "")
}

// setPRBase is setPR for a PR that targets (or was merged into) base.
func (r *replay) setPRBase(url, state, mergeable, base string) {
	r.t.Helper()
	b, _ := json.Marshal(sentinel.PR{State: state, Mergeable: mergeable, BaseRefName: base})
	if err := os.WriteFile(filepath.Join(r.ghDir, "view-"+filepath.Base(url)), b, 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *replay) remoteRev(branch string) string {
	out, _ := r.run(r.origin, "", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return out
}

// landedRange is the train's recorded from..to for task.
func (r *replay) landedRange(task string) (string, string) {
	r.t.Helper()
	es, err := r.a.Store.Train()
	if err != nil {
		r.t.Fatal(err)
	}
	for _, e := range es {
		if e.Task == task && e.State == store.TrainOK {
			from, to, ok := strings.Cut(e.Note, "..")
			if !ok {
				r.t.Fatalf("%s: train note %q has no range", task, e.Note)
			}
			return from, to
		}
	}
	r.t.Fatalf("%s has not landed", task)
	return "", ""
}

func (r *replay) trainState(task string) string {
	r.t.Helper()
	es, err := r.a.Store.Train()
	if err != nil {
		r.t.Fatal(err)
	}
	for _, e := range es {
		if e.Task == task {
			return e.State
		}
	}
	return ""
}

func (r *replay) task(id string) store.Task {
	r.t.Helper()
	tk, err := r.a.Store.Task(id)
	if err != nil {
		r.t.Fatal(err)
	}
	return tk
}

func (r *replay) land(want ...string) {
	r.t.Helper()
	rs, err := r.a.Land()
	if err != nil {
		r.t.Fatalf("Land: %v", err)
	}
	var got []string
	for _, res := range rs {
		if res.State != store.TrainOK {
			r.t.Fatalf("land %s: %s %s", res.Task, res.State, res.Note)
		}
		got = append(got, res.Task)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		r.t.Fatalf("landed %v, want %v", got, want)
	}
}

// newReplay starts saddle in a temp repo whose local main is one commit
// behind origin/main (m0), with a fake tmux and a fake gh on PATH. other is a
// clone of origin to act on GitHub's side from.
func newReplay(t *testing.T) (r *replay, other, m0 string, ft *replayTmux) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SADDLE_ROOT", "")
	t.Setenv("SADDLE_TASK", "")
	t.Setenv("SADDLE_TRAIN", "")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	r = &replay{t: t}

	// The fake gh logs its calls, numbers created PRs, and answers `pr view`
	// from a per-PR file, else as an open, mergeable PR.
	r.ghDir = t.TempDir()
	log := filepath.Join(r.ghDir, "gh.log")
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
case "$1 $2" in
"pr create")
	n=$(grep -c '^pr create' "` + log + `")
	echo "https://github.com/o/r/pull/$n" ;;
"pr view")
	f="` + r.ghDir + `/view-$(basename "$3")"
	if [ -f "$f" ]; then cat "$f"; else echo '{"state":"OPEN","mergeable":"MERGEABLE"}'; fi ;;
"pr edit")
	f="` + r.ghDir + `/view-$(basename "$3")"
	if [ "$4" = "--base" ] && [ -f "$f" ] && grep -q '"CLOSED"\|"MERGED"' "$f"; then
		echo "GraphQL: Cannot change the base branch of a closed pull request. (updatePullRequest)" >&2
		exit 1
	fi ;;
esac
`
	if err := os.WriteFile(filepath.Join(r.ghDir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", r.ghDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// 1. Local main is one commit behind origin/main.
	root := t.TempDir()
	r.git(root, "", "init", "-q", "-b", "main")
	r.commit(root, "", "scaffold", map[string]string{"README.md": "scaffold\n"})
	r.origin = t.TempDir()
	r.git(r.origin, "", "init", "-q", "--bare", "-b", "main")
	r.git(root, "", "remote", "add", "origin", r.origin)
	r.git(root, "", "push", "-q", "origin", "main")
	other = filepath.Join(t.TempDir(), "other")
	r.git(t.TempDir(), "", "clone", "-q", r.origin, other)
	r.commit(other, "", "M0 (#55)", map[string]string{"m0.txt": "m0\n"})
	r.git(other, "", "push", "-q", "origin", "main")
	m0 = r.git(other, "", "rev-parse", "HEAD")

	a, err := app.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	r.a = a
	ft = &replayTmux{windows: map[string]string{}}
	a.Tmux = ft
	a.Cfg.Test.Cmd = "true"
	a.Cfg.CloseOnLand = true
	integ := a.Cfg.Integration

	// Start saddle: the orchestrator task, the ref guard, integration.
	if _, _, err := a.Orchestrator(); err != nil {
		t.Fatal(err)
	}
	if got := r.git(root, "", "rev-parse", integ); got != m0 {
		t.Fatalf("integration cut from %s, want origin/main %s", got, m0)
	}
	return r, other, m0, ft
}

// TestReplayStackIncident replays the 2026-10-01 dogfood run (#83, #94)
// against a temp repo, a bare origin, a fake tmux and a fake gh: a stale local
// base, a squash-merged bottom PR, the orchestrator merging origin/main into
// integration and a worker force-pushing another task's branch. Saddle must
// refuse the out-of-train ref writes, flag the stack, restack it, and leave
// one linear stack whose PRs each show exactly their own task's work.
func TestReplayStackIncident(t *testing.T) {
	r, other, m0, ft := newReplay(t)
	a, root, integ := r.a, r.a.Root, r.a.Cfg.Integration
	a.Cfg.Train.Output = "single" // pins the one linear stack this test was written for (#52)

	type work struct {
		id, title, claim, file string
	}
	tasks := []work{
		{"t1", "usage", "usage/**", "usage/usage.go"},
		{"t2", "planner", "planner/**", "planner/planner.go"},
		{"t3", "git watcher", "watcher/**", "watcher/watcher.go"},
	}
	// Each task branches from origin's base, commits as itself and calls done.
	spawn := func(w work, base string) {
		t.Helper()
		tk, err := a.Spawn(app.SpawnReq{ID: w.id, Title: w.title, Parent: app.OrchestratorID, Claims: []string{w.claim}})
		if err != nil {
			t.Fatalf("spawn %s: %v", w.id, err)
		}
		if b := r.git(tk.Worktree, w.id, "merge-base", "HEAD", "origin/main"); b != base {
			t.Fatalf("%s branched from %s, not origin/main %s", w.id, b, base)
		}
		r.commit(tk.Worktree, w.id, w.title, map[string]string{w.file: "package x // " + w.title + "\n"})
		if err := a.Done(w.id, w.title+" summary"); err != nil {
			t.Fatalf("done %s: %v", w.id, err)
		}
	}
	// 2. t1..t3 commit and call done; land them, then open the PRs.
	for _, w := range tasks {
		spawn(w, m0)
	}
	r.land("t1", "t2", "t3")
	if _, err := a.PRs(); err != nil {
		t.Fatalf("PRs: %v", err)
	}
	t1 := r.task("t1")
	_, t1Landed := r.landedRange("t1")
	if t1.PR == "" || r.remoteRev(t1.Branch) != t1Landed {
		t.Fatalf("t1 PR %q, remote %s = %q, landed %s", t1.PR, t1.Branch, r.remoteRev(t1.Branch), t1Landed)
	}

	// 3. On origin, t1's PR is squash-merged and main moves on.
	r.git(other, "", "fetch", "-q", "origin")
	r.git(other, "", "merge", "-q", "--squash", "origin/"+t1.Branch)
	r.git(other, "", "commit", "-qm", "usage (#61)")
	r.commit(other, "", "unrelated fix", map[string]string{"fix.txt": "fix\n"})
	r.git(other, "", "push", "-q", "origin", "main")
	newMain := r.git(other, "", "rev-parse", "HEAD")
	r.setPR(t1.PR, "MERGED", "UNKNOWN")
	ghMerged := len(r.ghLog())

	// 4. The orchestrator merges origin/main into integration by hand, and t2
	// force-pushes t1's branch the way t5 did. Both must be refused.
	integBefore := r.git(root, "", "rev-parse", integ)
	r.git(root, app.OrchestratorID, "fetch", "-q", "origin")
	r.git(root, app.OrchestratorID, "checkout", "-q", integ)
	if out, err := r.run(root, app.OrchestratorID, "merge", "--no-edit", "origin/main"); err == nil {
		t.Errorf("orchestrator merged origin/main into %s: %s", integ, out)
	}
	_, _ = r.run(root, app.OrchestratorID, "merge", "--abort")
	r.git(root, app.OrchestratorID, "checkout", "-q", "-f", "main")
	if got := r.git(root, "", "rev-parse", integ); got != integBefore {
		t.Fatalf("orchestrator moved %s to %s", integ, got)
	}
	t2Landed := r.git(root, "", "rev-parse", r.task("t2").Branch)
	if out, err := r.run(root, "t2", "push", "--force", "origin", t2Landed+":refs/heads/"+t1.Branch); err == nil {
		t.Errorf("t2 force-pushed %s: %s", t1.Branch, out)
	}

	// The sentinel flags the stack; prs refuses to build on it.
	s := &sentinel.Sentinel{App: a, GH: &sentinel.GH{Dir: root}}
	rep, err := s.Check()
	if err != nil {
		t.Fatalf("sentinel check: %v", err)
	}
	if !rep.AtRisk {
		t.Fatalf("sentinel did not flag the stack: %+v", rep)
	}
	if _, flagged, _ := a.Flag(); !flagged {
		t.Fatal("stack not flagged")
	}
	before := r.git(r.origin, "", "for-each-ref", "refs/heads")
	if _, err := a.PRs(); err == nil || !strings.Contains(err.Error(), "restack") {
		t.Fatalf("PRs on a flagged stack: err = %v", err)
	}
	if after := r.git(r.origin, "", "for-each-ref", "refs/heads"); after != before {
		t.Fatalf("PRs pushed while flagged:\n%s\nwas\n%s", after, before)
	}

	// 5. Restack; the sentinel sees a sound stack and lifts the flag; prs.
	res, err := a.Restack()
	if err != nil {
		t.Fatalf("Restack: %v", err)
	}
	// GitHub said t1's PR merged into main, so t1 already left the stack.
	if len(res.Moves) == 0 || r.trainState("t1") != app.TrainMerged {
		t.Fatalf("restack moved %v; t1's train row is %q, want merged", res.Moves, r.trainState("t1"))
	}
	if rep, err := s.Check(); err != nil || rep.AtRisk {
		t.Fatalf("sentinel after restack: %+v, %v", rep, err)
	}
	if _, err := a.PRs(); err != nil {
		t.Fatalf("PRs after restack: %v", err)
	}

	// 6. t4 lands on top of the restacked stack.
	t4 := work{"t4", "ci watcher", "ciwatch/**", "ciwatch/ciwatch.go"}
	spawn(t4, newMain)
	r.land("t4")
	if _, err := a.PRs(); err != nil {
		t.Fatalf("PRs after t4: %v", err)
	}
	tasks = append(tasks, t4)

	// Invariants.
	r.git(root, "", "fetch", "-q", "origin")
	if got := r.git(root, "", "rev-parse", "origin/main"); got != newMain {
		t.Fatalf("origin/main = %s, want %s", got, newMain)
	}
	if b := r.git(root, "", "merge-base", "origin/main", integ); b != newMain {
		t.Errorf("integration does not descend from origin/main: merge-base %s", b)
	}
	if m := r.git(root, "", "rev-list", "--merges", "origin/main.."+integ); m != "" {
		t.Errorf("merge commits in origin/main..%s: %s", integ, m)
	}
	if n := r.git(root, "", "rev-list", "--count", "origin/main.."+integ); n != "3" {
		t.Errorf("integration has %s commits over origin/main, want 3 (t2, t3, t4)", n)
	}
	for _, w := range tasks {
		if n := r.git(root, "", "rev-list", "--count", integ, "--", w.file); n != "1" {
			t.Errorf("%s's change appears in %s commits of integration, want 1", w.id, n)
		}
	}

	// Every branch sits on its landed commit, locally and on origin. t1 merged,
	// so it keeps the commit it landed and pushed before the squash merge.
	for _, w := range tasks {
		tk := r.task(w.id)
		want := t1Landed
		if w.id != "t1" {
			_, want = r.landedRange(w.id)
		}
		if got := r.git(root, "", "rev-parse", "refs/heads/"+tk.Branch); got != want {
			t.Errorf("%s: local %s = %s, landed %s", w.id, tk.Branch, got, want)
		}
		if got := r.remoteRev(tk.Branch); got != want {
			t.Errorf("%s: origin %s = %s, landed %s", w.id, tk.Branch, got, want)
		}
	}

	// Each open PR's base..head is exactly its task's commit; t1's PR is left alone.
	bases := map[string]string{}
	heads := map[string]string{}
	created := 0
	for _, c := range r.ghLog() {
		f := strings.Fields(c)
		switch {
		case strings.HasPrefix(c, "pr create "):
			created++
			url := fmt.Sprintf("https://github.com/o/r/pull/%d", created)
			for i := 2; i+1 < len(f); i++ {
				switch f[i] {
				case "--base":
					bases[url] = f[i+1]
				case "--head":
					heads[url] = f[i+1]
				}
			}
		case strings.HasPrefix(c, "pr edit ") && len(f) >= 5 && f[3] == "--base":
			bases[f[2]] = f[4]
		}
	}
	if created != 4 {
		t.Errorf("gh opened %d PRs, want 4", created)
	}
	for _, w := range tasks[1:] {
		tk := r.task(w.id)
		if heads[tk.PR] != tk.Branch {
			t.Errorf("%s: PR %s head = %q, want %s", w.id, tk.PR, heads[tk.PR], tk.Branch)
			continue
		}
		got := r.git(r.origin, "", "log", "--format=%s", bases[tk.PR]+".."+tk.Branch)
		if got != w.title {
			t.Errorf("%s: PR %s %s..%s = %q, want only %q", w.id, tk.PR, bases[tk.PR], tk.Branch, got, w.title)
		}
	}
	if b := bases[r.task("t2").PR]; b != a.Cfg.Base {
		t.Errorf("t2's PR targets %s, want %s", b, a.Cfg.Base)
	}
	for _, c := range r.ghLog()[ghMerged:] {
		if strings.HasPrefix(c, "pr edit "+t1.PR+" ") || strings.Contains(c, "--head "+t1.Branch) {
			t.Errorf("merged t1 PR touched after its merge: %s", c)
		}
	}

	// Step 4's ref writes were refused and recorded; the sentinel flagged the
	// stack once and cleared it, labels included.
	evs, err := a.Store.Events(-1)
	if err != nil {
		t.Fatal(err)
	}
	denied := map[string]bool{}
	kinds := map[string]int{}
	for _, e := range evs {
		kinds[e.Kind]++
		if e.Kind != refguard.KindDenied {
			continue
		}
		var re refguard.Event
		if err := json.Unmarshal([]byte(e.Data), &re); err != nil {
			t.Fatalf("ref event %q: %v", e.Data, err)
		}
		denied[e.Task+" "+strings.TrimPrefix(re.Ref, "refs/heads/")] = true
	}
	for _, want := range []string{app.OrchestratorID + " " + integ, "t2 " + t1.Branch} {
		if !denied[want] {
			t.Errorf("no %s event for %s; denied: %v", refguard.KindDenied, want, denied)
		}
	}
	if kinds[sentinel.EventAtRisk] != 1 || kinds[sentinel.EventClear] != 1 {
		t.Errorf("sentinel events: %d %s, %d %s; want 1 each",
			kinds[sentinel.EventAtRisk], sentinel.EventAtRisk, kinds[sentinel.EventClear], sentinel.EventClear)
	}
	if _, flagged, _ := a.Flag(); flagged {
		t.Error("stack still flagged")
	}
	added, removed := map[string]bool{}, map[string]bool{}
	for _, c := range r.ghLog() {
		f := strings.Fields(c)
		if len(f) == 5 && f[0]+" "+f[1] == "pr edit" && f[4] == sentinel.Label {
			switch f[3] {
			case "--add-label":
				added[f[2]] = true
			case "--remove-label":
				removed[f[2]] = true
			}
		}
	}
	// Nothing here needed a human decision, so nothing was labeled (#119.7).
	if len(added) != 0 || len(removed) != 0 {
		t.Errorf("%s labels added %v, removed %v; want none", sentinel.Label, added, removed)
	}

	// No phantom tasks, no leftover worktrees.
	ts, err := a.Store.Tasks()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, tk := range ts {
		ids = append(ids, tk.ID+"="+tk.Status)
	}
	sort.Strings(ids)
	if got, want := strings.Join(ids, " "), "t0=running t1=landed t2=landed t3=landed t4=landed"; got != want {
		t.Errorf("tasks = %s, want %s", got, want)
	}
	if wts := r.git(root, "", "worktree", "list", "--porcelain"); strings.Count(wts, "worktree ") != 1 {
		t.Errorf("leftover worktrees:\n%s", wts)
	}
	if es, _ := os.ReadDir(filepath.Join(root, ".saddle", "worktrees")); len(es) != 0 {
		t.Errorf("leftover worktree dirs: %v", es)
	}
	if len(ft.windows) != 0 {
		t.Errorf("leftover windows: %v", ft.windows)
	}
}

// TestReplayGitHubHumansIncident replays 2026-10-02 (#119, #123): the owner
// used GitHub like a human. Stacked PRs were merged into each other and then
// squash-merged into main, a PR was closed, merged branches were deleted on
// GitHub and locally (plus one open layer's branch), a task with a PR and a
// landed train row was killed, main moved, and two tasks landed in one batch
// on an old binary that recorded the same single SHA for both. The stack has
// to clear with nothing but restack, and no state.db edit: no needs-human
// labels, no frozen land for independent work, no PR touched once it left the
// stack, and every task that is still open gets exactly its own work in its PR.
func TestReplayGitHubHumansIncident(t *testing.T) {
	r, other, _, _ := newReplay(t)
	a, root, integ := r.a, r.a.Root, r.a.Cfg.Integration
	a.Cfg.Train.Output = "single" // pins the one linear stack this test was written for (#52)
	file := func(id string) string { return id + "/" + id + ".go" }
	spawn := func(id string) {
		t.Helper()
		tk, err := a.Spawn(app.SpawnReq{ID: id, Title: "work " + id, Parent: app.OrchestratorID, Claims: []string{id + "/**"}})
		if err != nil {
			t.Fatalf("spawn %s: %v", id, err)
		}
		r.commit(tk.Worktree, id, "work "+id, map[string]string{file(id): "package " + id + "\n"})
		if err := a.Done(id, "work "+id); err != nil {
			t.Fatalf("done %s: %v", id, err)
		}
	}
	for _, id := range []string{"t1", "t2", "t3", "t4", "t5", "t6"} {
		spawn(id)
		r.land(id)
	}
	// t7 and t8 land in one batch; the old binary noted the same SHA for both.
	spawn("t7")
	spawn("t8")
	r.land("t7", "t8")
	tip := r.git(root, "", "rev-parse", integ)
	for _, id := range []string{"t7", "t8"} {
		if err := a.Store.SetTrain(id, store.TrainOK, tip[:7], false); err != nil {
			t.Fatal(err)
		}
	}
	if urls, err := a.PRs(); err != nil || len(urls) != 8 {
		t.Fatalf("PRs = %v, %v", urls, err)
	}
	t1, t2, t3, t5 := r.task("t1"), r.task("t2"), r.task("t3"), r.task("t5")
	t4 := r.task("t4")

	// On GitHub: t2's PR is merged into t1's branch, t1's PR is squash-merged
	// into main, main gets a hotfix, and GitHub deletes both merged branches.
	r.git(other, "", "fetch", "-q", "origin")
	r.git(other, "", "checkout", "-q", "-B", "b1", "origin/"+t1.Branch)
	r.git(other, "", "merge", "-q", "--no-ff", "-m", "Merge pull request #2", "origin/"+t2.Branch)
	r.git(other, "", "push", "-q", "origin", "HEAD:refs/heads/"+t1.Branch)
	r.git(other, "", "checkout", "-q", "main")
	r.git(other, "", "merge", "-q", "--squash", "b1")
	r.git(other, "", "commit", "-qm", "work t1 (#1)")
	r.commit(other, "", "hotfix", map[string]string{"hotfix.txt": "fix\n"})
	r.git(other, "", "push", "-q", "origin", "main")
	r.git(other, "", "push", "-q", "origin", "--delete", t1.Branch, t2.Branch)
	r.setPRBase(t1.PR, "MERGED", "UNKNOWN", "main")
	r.setPRBase(t2.PR, "MERGED", "UNKNOWN", t1.Branch)
	// The owner closes t3's PR; the orchestrator kills t4, leaving its PR url
	// and its landed train row.
	r.setPRBase(t3.PR, "CLOSED", "UNKNOWN", t2.Branch)
	if err := a.Kill("t4", false); err != nil {
		t.Fatal(err)
	}
	// Branches are deleted locally, one of them an open layer's.
	for _, b := range []string{t1.Branch, t2.Branch, t5.Branch} {
		cmd := exec.Command("git", "-C", root, "branch", "-D", b)
		cmd.Env = append(os.Environ(), "SADDLE_TRAIN=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("delete %s: %v: %s", b, err, out)
		}
	}
	ghBefore := len(r.ghLog())

	// The sentinel asks for a restack but labels nothing, and t1..t4 leave
	// the stack for good.
	s := &sentinel.Sentinel{App: a, GH: &sentinel.GH{Dir: root}}
	rep, err := s.Check()
	if err != nil {
		t.Fatalf("sentinel: %v", err)
	}
	if !rep.AtRisk || rep.Task != "t5" || len(rep.PRs) != 0 {
		t.Fatalf("sentinel report = %+v, want t5 needing a restack and no labels", rep)
	}
	for id, want := range map[string]string{"t1": app.TrainMerged, "t2": app.TrainMerged, "t3": app.TrainSuperseded, "t4": app.TrainSuperseded} {
		if got := r.trainState(id); got != want {
			t.Errorf("%s's train row = %q, want %q", id, got, want)
		}
	}

	// Work that doesn't touch the stack still lands while it is flagged.
	spawn("t9")
	r.land("t9")

	// The orchestrator restacks. Nothing else is done by hand.
	res, err := a.Restack()
	if err != nil {
		t.Fatalf("Restack: %v", err)
	}
	if got := strings.Join(res.Superseded, ","); got != "t3,t4" {
		t.Errorf("restack superseded = %s, want t3,t4", got)
	}
	if rep, err := s.Check(); err != nil || rep.AtRisk {
		t.Fatalf("sentinel after restack: %+v, %v", rep, err)
	}
	urls, err := a.PRs()
	if err != nil {
		t.Fatalf("PRs after restack: %v", err)
	}
	if len(urls) != 5 {
		t.Errorf("PRs = %v, want t5..t9", urls)
	}
	if _, flagged, _ := a.Flag(); flagged {
		t.Error("stack still flagged")
	}

	// Integration is origin/main plus t5..t9, one commit each, linear.
	r.git(root, "", "fetch", "-q", "origin")
	if b, m := r.git(root, "", "merge-base", "origin/main", integ), r.git(root, "", "rev-parse", "origin/main"); b != m {
		t.Errorf("integration does not descend from origin/main")
	}
	if n := r.git(root, "", "rev-list", "--count", "origin/main.."+integ); n != "5" {
		t.Errorf("integration has %s commits over origin/main, want 5", n)
	}
	for _, id := range []string{"t3", "t4"} {
		if r.has(integ, file(id)) {
			t.Errorf("%s left the stack but its work is still on integration", id)
		}
	}
	for _, id := range []string{"t1", "t2"} {
		if !r.has("origin/main", file(id)) {
			t.Errorf("%s's merged work is not on origin/main", id)
		}
	}

	// Each open PR shows exactly its own task's commit, stacked t5..t9 on main.
	bases := map[string]string{}
	created := 0
	for _, c := range r.ghLog() {
		f := strings.Fields(c)
		switch {
		case strings.HasPrefix(c, "pr create "):
			created++
			for i := 2; i+1 < len(f); i++ {
				if f[i] == "--base" {
					bases[fmt.Sprintf("https://github.com/o/r/pull/%d", created)] = f[i+1]
				}
			}
		case strings.HasPrefix(c, "pr edit ") && len(f) >= 5 && f[3] == "--base":
			bases[f[2]] = f[4]
		}
	}
	want := a.Cfg.Base
	for _, id := range []string{"t5", "t6", "t7", "t8", "t9"} {
		tk := r.task(id)
		if tk.PR == "" {
			t.Errorf("%s has no PR", id)
			continue
		}
		if bases[tk.PR] != want {
			t.Errorf("%s's PR targets %q, want %q", id, bases[tk.PR], want)
		}
		if got := r.git(r.origin, "", "log", "--format=%s", bases[tk.PR]+".."+tk.Branch); got != "work "+id {
			t.Errorf("%s's PR shows %q, want only %q", id, got, "work "+id)
		}
		want = tk.Branch
	}

	// Once out of the stack, no PR was edited or labeled, and no PR ever was.
	for _, c := range r.ghLog()[ghBefore:] {
		for _, tk := range []store.Task{t1, t2, t3, t4} {
			if strings.HasPrefix(c, "pr edit "+tk.PR+" ") {
				t.Errorf("%s left the stack but its PR was edited: %s", tk.ID, c)
			}
		}
	}
	for _, c := range r.ghLog() {
		if strings.Contains(c, "--add-label") {
			t.Errorf("labeled without a conflict: %s", c)
		}
	}
}

// has reports whether rev has path.
func (r *replay) has(rev, path string) bool {
	_, err := r.run(r.a.Root, "", "cat-file", "-e", rev+":"+path)
	return err == nil
}
