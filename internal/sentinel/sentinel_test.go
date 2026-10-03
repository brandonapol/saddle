package sentinel

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

type fakeTmux struct{ n int }

func (f *fakeTmux) HasSession() bool                          { return true }
func (f *fakeTmux) NewSession(n, d, c string) (string, error) { return f.NewWindow(n, d, c) }
func (f *fakeTmux) NewWindow(string, string, string) (string, error) {
	f.n++
	return fmt.Sprintf("@%d", f.n), nil
}
func (f *fakeTmux) KillWindow(string) error             { return nil }
func (f *fakeTmux) Alive(string) bool                   { return false }
func (f *fakeTmux) SendText(string, string) error       { return nil }
func (f *fakeTmux) SendKeys(string, ...string) error    { return nil }
func (f *fakeTmux) Capture(string, int) (string, error) { return "", nil }
func (f *fakeTmux) KillSession() error                  { return nil }

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitx.Run(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, []byte(body), 0o644))
}

func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", msg)
}

// fakeGH is a gh on PATH that logs its calls, numbers created PRs, and
// answers `pr view` from a per-PR file, else as an open, mergeable PR.
type fakeGH struct {
	dir string
	t   *testing.T
}

func newFakeGH(t *testing.T) *fakeGH {
	t.Helper()
	bin := t.TempDir()
	log := filepath.Join(bin, "gh.log")
	script := `#!/bin/sh
echo "$*" >> "` + log + `"
case "$1 $2" in
"pr create")
	n=$(grep -c '^pr create' "` + log + `")
	echo "https://github.com/o/r/pull/$n" ;;
"pr view")
	f="` + bin + `/view-$(basename "$3")"
	if [ -f "$f" ]; then cat "$f"; else echo '{"state":"OPEN","mergeable":"MERGEABLE"}'; fi ;;
esac
`
	must(t, os.WriteFile(filepath.Join(bin, "gh"), []byte(script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &fakeGH{dir: bin, t: t}
}

func (g *fakeGH) log() []string {
	b, err := os.ReadFile(filepath.Join(g.dir, "gh.log"))
	if os.IsNotExist(err) {
		return nil
	}
	must(g.t, err)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// setPR makes `gh pr view` report state and mergeable for the PR at url.
func (g *fakeGH) setPR(url, state, mergeable string) {
	must(g.t, os.WriteFile(filepath.Join(g.dir, "view-"+filepath.Base(url)),
		[]byte(fmt.Sprintf(`{"state":%q,"mergeable":%q}`, state, mergeable)), 0o644))
}

func setup(t *testing.T) (*app.App, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SADDLE_ROOT", "")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	write(t, root, "README.md", "hi\n")
	commitAll(t, root, "init")
	origin := t.TempDir()
	git(t, origin, "init", "-q", "--bare", "-b", "main")
	git(t, root, "remote", "add", "origin", origin)
	git(t, root, "push", "-q", "origin", "main")
	git(t, root, "fetch", "-q", "origin")
	a, err := app.Open(root)
	must(t, err)
	t.Cleanup(func() { a.Close() })
	a.Cfg.Test.Cmd = app.NoTestCmd
	a.Cfg.CloseOnLand = true
	a.Tmux = &fakeTmux{}
	return a, origin
}

func landTask(t *testing.T, a *app.App, id, file string) store.Task {
	t.Helper()
	tk, err := a.Spawn(app.SpawnReq{ID: id, Title: file})
	must(t, err)
	write(t, tk.Worktree, file+".txt", file+"\n")
	t.Setenv("SADDLE_TASK", tk.ID) // commit as the task, as the ref guard expects
	commitAll(t, tk.Worktree, file)
	must(t, a.Done(tk.ID, file+" summary"))
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

func events(t *testing.T, a *app.App, kind string) []store.Event {
	t.Helper()
	es, err := a.Store.Events(1000)
	must(t, err)
	var out []store.Event
	for _, e := range es {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func trainState(t *testing.T, a *app.App, task string) string {
	t.Helper()
	es, err := a.Store.Train()
	must(t, err)
	for _, e := range es {
		if e.Task == task {
			return e.State
		}
	}
	return ""
}

func hasCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

// Replays the 2026-10-01 fork (#83, #94): t1 and t2 are rewritten onto a new
// main, integration gets main merged in, and t4 and t6 are rebuilt on the old
// lineage. GitHub reports t4's PR as conflicting. The sentinel must flag t4
// and everything above it, label those PRs, record one event, notify the
// orchestrator and freeze prs and land, but never restack. Once the stack is
// restacked it checks clean and lifts all of that.
func TestSentinelFlagsForkedStackUntilRestack(t *testing.T) {
	a, _ := setup(t)
	gh := newFakeGH(t)
	t1 := landTask(t, a, "t1", "usage")
	t2 := landTask(t, a, "t2", "planner")
	t4 := landTask(t, a, "t4", "ciwatch")
	t6 := landTask(t, a, "t6", "tui")
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	for _, tk := range []*store.Task{&t1, &t2, &t4, &t6} {
		*tk, _ = a.Store.Task(tk.ID)
	}
	c1, c2, c4, c6 := git(t, a.Root, "rev-parse", t1.Branch), git(t, a.Root, "rev-parse", t2.Branch),
		git(t, a.Root, "rev-parse", t4.Branch), git(t, a.Root, "rev-parse", t6.Branch)

	// main moves; t1 and t2 are rebased onto it outside the train.
	write(t, a.Root, "main.txt", "main\n")
	commitAll(t, a.Root, "main moves")
	m := git(t, a.Root, "rev-parse", "HEAD")
	git(t, a.Root, "push", "-q", "origin", "main")
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, a.Root, "worktree", "add", "-q", "--detach", wt, m)
	git(t, wt, "cherry-pick", c1)
	n1 := git(t, wt, "rev-parse", "HEAD")
	git(t, wt, "cherry-pick", c2)
	n2 := git(t, wt, "rev-parse", "HEAD")
	// main is merged into the old lineage, and t4 and t6 land on top of that.
	git(t, wt, "checkout", "-q", "--detach", c2)
	git(t, wt, "merge", "-q", "--no-ff", "-m", "Merge origin/main into saddle/integration", m)
	g := git(t, wt, "rev-parse", "HEAD")
	git(t, wt, "cherry-pick", c4)
	n4 := git(t, wt, "rev-parse", "HEAD")
	git(t, wt, "cherry-pick", c6)
	n6 := git(t, wt, "rev-parse", "HEAD")
	for _, r := range []struct {
		tk       store.Task
		from, to string
	}{{t1, m, n1}, {t2, n1, n2}, {t4, g, n4}, {t6, n4, n6}} {
		t.Setenv("SADDLE_TASK", r.tk.ID) // each branch moves as its owner
		git(t, a.Root, "update-ref", "refs/heads/"+r.tk.Branch, r.to)
		must(t, a.Store.SetTrain(r.tk.ID, store.TrainOK, r.from+".."+r.to, false))
	}
	// Only the train may move integration.
	t.Setenv("SADDLE_TRAIN", "1")
	git(t, a.Root, "update-ref", "refs/heads/"+a.Cfg.Integration, n6)
	t.Setenv("SADDLE_TRAIN", "")
	gh.setPR(t4.PR, "OPEN", "CONFLICTING")
	before := len(gh.log())

	s := New(a)
	rep, err := s.Check()
	must(t, err)
	if !rep.AtRisk || rep.Task != t4.ID {
		t.Fatalf("report = %+v, want t4 at risk", rep)
	}
	calls := gh.log()[before:]
	for _, tk := range []store.Task{t4, t6} {
		if !hasCall(calls, "pr edit "+tk.PR+" --add-label "+Label) {
			t.Fatalf("%s's PR not labeled: %v", tk.ID, calls)
		}
	}
	for _, tk := range []store.Task{t1, t2} {
		for _, c := range calls {
			if strings.HasPrefix(c, "pr edit "+tk.PR+" ") {
				t.Fatalf("healthy %s's PR edited: %s", tk.ID, c)
			}
		}
	}
	if evs := events(t, a, EventAtRisk); len(evs) != 1 || evs[0].Task != t4.ID || !strings.Contains(evs[0].Data, "conflict") {
		t.Fatalf("stack_at_risk events = %+v", evs)
	}
	ns, err := a.Store.TakeNotices(app.OrchestratorID, false)
	must(t, err)
	var note string
	for _, n := range ns {
		if strings.Contains(n.Text, "restack") {
			note = n.Text
		}
	}
	if note == "" || !strings.Contains(note, t4.ID) {
		t.Fatalf("orchestrator not told to restack: %+v", ns)
	}
	if got := git(t, a.Root, "rev-parse", t1.Branch); got != n1 {
		t.Fatal("sentinel moved t1's branch; it must never restack")
	}

	// A second cycle with nothing new records nothing new.
	before = len(gh.log())
	if _, err := s.Check(); err != nil {
		t.Fatal(err)
	}
	if evs := events(t, a, EventAtRisk); len(evs) != 1 {
		t.Fatalf("second cycle wrote another event: %+v", evs)
	}
	for _, c := range gh.log()[before:] {
		if strings.Contains(c, "--add-label") {
			t.Fatalf("second cycle relabeled: %s", c)
		}
	}

	// While flagged, prs publishes only the layers below t4 (#119.4).
	before = len(gh.log())
	_, err = a.PRs()
	if err == nil || !strings.Contains(err.Error(), "restack") || !strings.Contains(err.Error(), t4.ID) {
		t.Fatalf("PRs while flagged: err = %v", err)
	}
	for _, c := range gh.log()[before:] {
		for _, tk := range []store.Task{t4, t6} {
			if strings.HasPrefix(c, "pr edit "+tk.PR) || strings.HasPrefix(c, "pr create") {
				t.Fatalf("prs touched %s while it is flagged: %s", tk.ID, c)
			}
		}
	}

	// The orchestrator restacks; GitHub sees t4's PR as mergeable again.
	if _, err := a.Restack(); err != nil {
		t.Fatal(err)
	}
	gh.setPR(t4.PR, "OPEN", "MERGEABLE")
	before = len(gh.log())
	rep, err = s.Check()
	must(t, err)
	if rep.AtRisk {
		t.Fatalf("still at risk after restack: %+v", rep)
	}
	calls = gh.log()[before:]
	for _, tk := range []store.Task{t4, t6} {
		if !hasCall(calls, "pr edit "+tk.PR+" --remove-label "+Label) {
			t.Fatalf("%s's label not cleared: %v", tk.ID, calls)
		}
	}
	if len(events(t, a, EventClear)) != 1 {
		t.Fatal("clearing not recorded")
	}
	if _, err := a.PRs(); err != nil {
		t.Fatalf("PRs after the stack checks clean: %v", err)
	}
}

// A squash-merged bottom PR puts the PR above it at risk, not the merged one.
func TestSentinelFlagsAboveSquashMergedBottom(t *testing.T) {
	a, origin := setup(t)
	gh := newFakeGH(t)
	t1 := landTask(t, a, "t1", "one")
	t2 := landTask(t, a, "t2", "two")
	t3 := landTask(t, a, "t3", "three")
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	for _, tk := range []*store.Task{&t1, &t2, &t3} {
		*tk, _ = a.Store.Task(tk.ID)
	}
	other := filepath.Join(t.TempDir(), "other")
	git(t, a.Root, "clone", "-q", origin, other)
	git(t, other, "merge", "-q", "--squash", "origin/"+t1.Branch)
	git(t, other, "commit", "-qm", "one (#1)")
	git(t, other, "push", "-q", "origin", "main")
	gh.setPR(t1.PR, "MERGED", "UNKNOWN")
	before := len(gh.log())

	// A PR merged into main is not an incident (#119.2): t1 leaves the stack,
	// and the PRs above it only need a restack, which needs no human, so
	// nothing is labeled needs-human (#119.7).
	s := New(a)
	rep, err := s.Check()
	must(t, err)
	if !rep.AtRisk || rep.Task != t2.ID || !strings.Contains(strings.Join(rep.Hits, "\n"), "origin/main has") {
		t.Fatalf("report = %+v, want t2 needing a restack after t1 merged", rep)
	}
	if len(rep.PRs) != 0 {
		t.Fatalf("labeled %v for a routine restack", rep.PRs)
	}
	for _, c := range gh.log()[before:] {
		if strings.Contains(c, "--add-label") {
			t.Fatalf("labeled without a conflict: %s", c)
		}
	}
	if st := trainState(t, a, t1.ID); st != app.TrainMerged {
		t.Fatalf("t1's train row = %q, want merged", st)
	}

	if _, err := a.Restack(); err != nil {
		t.Fatal(err)
	}
	if rep, err = s.Check(); err != nil || rep.AtRisk {
		t.Fatalf("after restack: %+v, %v", rep, err)
	}
	if _, ok, _ := a.Flag(); ok {
		t.Fatal("flag not cleared")
	}
}

// The sentinel skips a cycle rather than read refs the train is moving.
func TestSentinelSkipsWhileTrainBusy(t *testing.T) {
	a, _ := setup(t)
	newFakeGH(t)
	unlock, ok, err := a.TryLockTrain()
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	defer unlock()
	rep, err := New(a).Check()
	if err != nil || !rep.Busy {
		t.Fatalf("check with the train busy: %+v, %v", rep, err)
	}
}

// A cycle that finds the train busy (automerge or a land holds the lock) is
// retried after BusyRetry, not a full Interval: saddle up starts the sentinel
// next to automerge, and losing the first race left the stack unchecked for
// minutes.
func TestSentinelRunRetriesSoonWhenTrainBusy(t *testing.T) {
	a, _ := setup(t)
	gh := newFakeGH(t)
	t1 := landTask(t, a, "t1", "one")
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	t1, _ = a.Store.Task(t1.ID)
	gh.setPR(t1.PR, "OPEN", "CONFLICTING")

	unlock, ok, err := a.TryLockTrain()
	if err != nil || !ok {
		t.Fatalf("lock: %v %v", ok, err)
	}
	s := New(a)
	s.Interval = time.Hour
	s.BusyRetry = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = s.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	time.Sleep(50 * time.Millisecond)
	unlock()

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, flagged, err := a.Flag()
		must(t, err)
		if flagged {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("sentinel did not check again once the train lock was free")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// commentGH is GitHub through the fake gh, except PR comments, which it keeps
// in memory so tests can read them back.
type commentGH struct {
	*GH
	comments     map[string][]Comment
	n            int
	posts, edits int
}

func newCommentGH(a *app.App) *commentGH {
	return &commentGH{GH: &GH{Dir: a.Root}, comments: map[string][]Comment{}}
}

func (g *commentGH) Comments(url string) ([]Comment, error) {
	return append([]Comment(nil), g.comments[url]...), nil
}

func (g *commentGH) AddComment(url, body string) error {
	g.n++
	g.posts++
	g.comments[url] = append(g.comments[url], Comment{ID: fmt.Sprintf("IC_%d", g.n), Body: body})
	return nil
}

func (g *commentGH) EditComment(id, body string) error {
	g.edits++
	for url, cs := range g.comments {
		for i := range cs {
			if cs[i].ID == id {
				g.comments[url][i].Body = body
				return nil
			}
		}
	}
	return fmt.Errorf("no comment %s", id)
}

// only returns the one comment on url, failing on none or several.
func (g *commentGH) only(t *testing.T, url string) string {
	t.Helper()
	cs := g.comments[url]
	if len(cs) != 1 {
		t.Fatalf("%s has %d comments, want 1: %+v", url, len(cs), cs)
	}
	return cs[0].Body
}

// Each PR the sentinel labels gets one comment saying which conflict needs a
// human, what to do, and what clears it (#118, #119.7). Repeat ticks, even
// from a restarted sentinel, leave it alone; a new cause edits it; clearing
// the stack turns it into a resolution note.
func TestSentinelExplainsNeedsHumanInPRComment(t *testing.T) {
	a, _ := setup(t)
	fake := newFakeGH(t)
	t1 := landTask(t, a, "t1", "one")
	t2 := landTask(t, a, "t2", "two")
	t3 := landTask(t, a, "t3", "three")
	if _, err := a.PRs(); err != nil {
		t.Fatal(err)
	}
	for _, tk := range []*store.Task{&t1, &t2, &t3} {
		*tk, _ = a.Store.Task(tk.ID)
	}
	fake.setPR(t2.PR, "OPEN", "CONFLICTING")

	gh := newCommentGH(a)
	s := New(a)
	s.GH = gh
	if _, err := s.Check(); err != nil {
		t.Fatal(err)
	}
	if len(gh.comments[t1.PR]) != 0 {
		t.Fatalf("healthy PR below the conflict commented on: %+v", gh.comments[t1.PR])
	}
	for _, tk := range []store.Task{t2, t3} {
		body := gh.only(t, tk.PR)
		for _, want := range []string{commentMarker, Label, t2.PR, "conflict", "restack", "clear"} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s's comment lacks %q:\n%s", tk.ID, want, body)
			}
		}
	}
	first := gh.only(t, t3.PR)

	// Repeat ticks, and a restarted sentinel, neither post nor edit.
	if _, err := s.Check(); err != nil {
		t.Fatal(err)
	}
	s2 := New(a)
	s2.GH = gh
	if _, err := s2.Check(); err != nil {
		t.Fatal(err)
	}
	if gh.posts != 2 || gh.edits != 0 {
		t.Fatalf("repeat ticks: %d posts, %d edits; want 2, 0", gh.posts, gh.edits)
	}

	// t3 conflicts too: its comment is edited, not doubled.
	fake.setPR(t3.PR, "OPEN", "CONFLICTING")
	if _, err := s2.Check(); err != nil {
		t.Fatal(err)
	}
	if body := gh.only(t, t3.PR); body == first || !strings.Contains(body, "This PR") {
		t.Fatalf("t3's comment not updated with the new cause:\n%s", body)
	}
	if gh.posts != 2 {
		t.Fatalf("changed cause posted a new comment: %d posts", gh.posts)
	}

	// Resolved: each comment becomes a resolution note.
	fake.setPR(t2.PR, "OPEN", "MERGEABLE")
	fake.setPR(t3.PR, "OPEN", "MERGEABLE")
	if rep, err := s2.Check(); err != nil || rep.AtRisk {
		t.Fatalf("after the conflicts resolved: %+v, %v", rep, err)
	}
	for _, tk := range []store.Task{t2, t3} {
		body := gh.only(t, tk.PR)
		if !strings.Contains(body, commentMarker) || !strings.Contains(body, "Cleared") {
			t.Fatalf("%s's comment not resolved:\n%s", tk.ID, body)
		}
	}
	if gh.posts != 2 {
		t.Fatalf("clearing posted new comments: %d posts", gh.posts)
	}
}

// GH reads, posts and edits PR comments through gh.
func TestGHComments(t *testing.T) {
	fake := newFakeGH(t)
	g := &GH{Dir: t.TempDir()}
	url := "https://github.com/o/r/pull/7"
	must(t, os.WriteFile(filepath.Join(fake.dir, "view-7"),
		[]byte(`{"comments":[{"id":"IC_1","body":"hi"},{"id":"IC_2","body":"`+commentMarker+` x"}]}`), 0o644))
	cs, err := g.Comments(url)
	must(t, err)
	if len(cs) != 2 || cs[1].ID != "IC_2" || !strings.Contains(cs[1].Body, commentMarker) {
		t.Fatalf("comments = %+v", cs)
	}
	must(t, g.AddComment(url, "posted"))
	must(t, g.EditComment("IC_2", "edited"))
	log := strings.Join(fake.log(), "\n")
	for _, want := range []string{"pr view " + url + " --json comments", "pr comment " + url + " --body posted",
		"updateIssueComment", "id=IC_2", "body=edited"} {
		if !strings.Contains(log, want) {
			t.Fatalf("gh log lacks %q:\n%s", want, log)
		}
	}
}
