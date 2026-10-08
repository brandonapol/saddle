package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// serverDies is the tmux server (or saddle's session) vanishing: every
// window is gone.
func serverDies(ft *fakeTmux) {
	ft.session = false
	ft.windows = map[string]bool{}
}

func launchScript(t *testing.T, a *App, id string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(a.stateDir("run", id), "launch.sh"))
	must(t, err)
	return string(b)
}

func runFile(t *testing.T, a *App, id, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(a.stateDir("run", id), name))
	must(t, err)
	return string(b)
}

// withTranscript makes task id's Claude session resumable: it has a session
// id and a transcript on disk.
func withTranscript(t *testing.T, a *App, id, session string) {
	t.Helper()
	tk, err := a.Store.Task(id)
	must(t, err)
	must(t, a.Store.SetField(id, "session_id", session))
	p := usage.ClaudeTranscriptPath(os.Getenv("CLAUDE_CONFIG_DIR"), tk.Worktree, session)
	must(t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(t, os.WriteFile(p, []byte("{}\n"), 0o644))
}

func countEvents(t *testing.T, a *App, kind, sub string) int {
	t.Helper()
	es, err := a.Store.Events(1000)
	must(t, err)
	n := 0
	for _, e := range es {
		if e.Kind == kind && strings.Contains(e.Data, sub) {
			n++
		}
	}
	return n
}

// #254: when the tmux server dies, every running task gets a new window in
// its worktree. A Claude task with a session resumes it; one without is
// relaunched with its brief and a note that work may already exist. The
// orchestrator hears about it once, as a digest item.
func TestResumeLostWindowsAfterServerDies(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	a, ft := setup(t)
	t1, err := a.Spawn(SpawnReq{Title: "one", Prompt: "do one", Claims: []string{"one/**"}})
	must(t, err)
	t2, err := a.Spawn(SpawnReq{Title: "two", Prompt: "do two", Claims: []string{"two/**"}})
	must(t, err)
	withTranscript(t, a, t1.ID, "sess-1")
	serverDies(ft)

	rs, err := a.ResumeLost(false)
	must(t, err)
	if len(rs) != 2 {
		t.Fatalf("resumed %+v, want both tasks", rs)
	}
	for _, id := range []string{t1.ID, t2.ID} {
		tk, _ := a.Store.Task(id)
		if !a.ownWindow(tk) || tk.Status != store.Running {
			t.Errorf("%s after resume: window %q alive=%v status %s", id, tk.Window, a.Tmux.Alive(tk.Window), tk.Status)
		}
		if countEvents(t, a, "resumed", "") == 0 {
			t.Errorf("no resumed event")
		}
	}
	if !ft.session {
		t.Error("the tmux session was not recreated")
	}
	if s := launchScript(t, a, t1.ID); !strings.Contains(s, "--resume 'sess-1'") {
		t.Errorf("t1 didn't resume its session:\n%s", s)
	}
	if s := launchScript(t, a, t2.ID); strings.Contains(s, "--resume") {
		t.Errorf("t2 has no session to resume:\n%s", s)
	}
	if p := runFile(t, a, t2.ID, "prompt.md"); !strings.Contains(p, "do two") || !strings.Contains(p, "may already exist") {
		t.Errorf("t2's relaunch prompt = %q, want its brief and a note about existing work", p)
	}
	if n := countEvents(t, a, EventNoticeDigest, "Resumed 2 tasks"); n != 1 {
		t.Errorf("digest notices about the resume = %d, want 1", n)
	}
	if n := countEvents(t, a, EventNoticeInterrupt, "Resumed"); n != 0 {
		t.Errorf("the resume interrupted the orchestrator")
	}
	// Nothing is lost a second time.
	if rs, err := a.ResumeLost(false); err != nil || len(rs) != 0 {
		t.Fatalf("second pass resumed %+v, %v", rs, err)
	}
}

// Killed tasks, tasks still spawning (no window yet) and done tasks are
// never resumed; paused tasks only when asked (saddle up's startup pass).
func TestResumeLostSkipsDeadAndPaused(t *testing.T) {
	a, ft := setup(t)
	k, err := a.Spawn(SpawnReq{Title: "killed"})
	must(t, err)
	must(t, a.Kill(k.ID, true))
	p, err := a.Spawn(SpawnReq{Title: "paused"})
	must(t, err)
	must(t, a.Store.CreateTask(store.Task{ID: "t9", Title: "spawning", Role: store.RoleWorker, Status: store.Running}))
	paused, err := a.Pause()
	must(t, err)
	if len(paused) != 1 || paused[0] != p.ID {
		t.Fatalf("paused %v, want [%s]", paused, p.ID)
	}
	serverDies(ft)
	if rs, err := a.ResumeLost(false); err != nil || len(rs) != 0 {
		t.Fatalf("watcher pass resumed %+v, %v", rs, err)
	}
	rs, err := a.ResumeLost(true)
	must(t, err)
	if len(rs) != 1 || rs[0].Task != p.ID {
		t.Fatalf("startup pass resumed %+v, want only %s", rs, p.ID)
	}
	if tk, _ := a.Store.Task(p.ID); tk.Status != store.Running {
		t.Fatalf("%s is %s after resume", p.ID, tk.Status)
	}
	if tk, _ := a.Store.Task(k.ID); tk.Status != store.Killed {
		t.Fatalf("killed task is %s", tk.Status)
	}
}

// saddle down pauses live tasks: claims and worktrees stay, uncommitted work
// is snapshotted, and the session is stopped.
func TestPauseKeepsClaimsAndSnapshots(t *testing.T) {
	a, _ := setup(t)
	tk, err := a.Spawn(SpawnReq{Title: "one", Claims: []string{"one/**"}})
	must(t, err)
	write(t, tk.Worktree, "one/wip.txt", "half done\n")
	ids, err := a.Down()
	must(t, err)
	if len(ids) != 1 || ids[0] != tk.ID {
		t.Fatalf("down paused %v", ids)
	}
	got, _ := a.Store.Task(tk.ID)
	if got.Status != StatusPaused {
		t.Fatalf("status %s, want paused", got.Status)
	}
	cl, _ := a.Store.Claims()
	if len(cl[tk.ID]) != 1 {
		t.Fatalf("claims after down: %v", cl[tk.ID])
	}
	if out := git(t, a.Root, "show", "refs/saddle/wip/"+tk.ID+":one/wip.txt"); out != "half done" {
		t.Fatalf("wip snapshot has %q", out)
	}
}

// Kill snapshots uncommitted work to refs/saddle/wip/<task> first, and gc
// keeps the snapshot.
func TestKillSnapshotsWIP(t *testing.T) {
	a, _ := setup(t)
	tk, err := a.Spawn(SpawnReq{Title: "one"})
	must(t, err)
	write(t, tk.Worktree, "README.md", "edited\n")
	write(t, tk.Worktree, "new.txt", "untracked\n")
	must(t, a.Kill(tk.ID, false))
	ref := "refs/saddle/wip/" + tk.ID
	if out := git(t, a.Root, "show", ref+":new.txt"); out != "untracked" {
		t.Fatalf("snapshot new.txt = %q", out)
	}
	if out := git(t, a.Root, "show", ref+":README.md"); out != "edited" {
		t.Fatalf("snapshot README.md = %q", out)
	}
	// The worktree itself is untouched.
	if d, _ := os.ReadFile(filepath.Join(tk.Worktree, "new.txt")); string(d) != "untracked\n" {
		t.Fatalf("worktree lost new.txt: %q", d)
	}
	ls, err := a.GC()
	must(t, err)
	for _, l := range ls {
		if l.Name == ref && l.Keep == "" {
			t.Fatalf("gc removed the wip snapshot: %+v", l)
		}
	}
	git(t, a.Root, "rev-parse", "--verify", ref)
}

// A running task with no window and no heartbeat for OrphanAfter is
// orphaned: shown as such and not counted against the concurrency limit.
func TestOrphanedTasks(t *testing.T) {
	a, ft := setup(t)
	tk, err := a.Spawn(SpawnReq{Title: "one"})
	must(t, err)
	if n, _ := a.activeWorkers(); n != 1 {
		t.Fatalf("active = %d", n)
	}
	serverDies(ft)
	if o, _ := a.Orphans(); o[tk.ID] {
		t.Fatal("orphaned right after its window vanished; heartbeat is fresh")
	}
	old := OrphanAfter
	OrphanAfter = -time.Minute
	t.Cleanup(func() { OrphanAfter = old })
	o, err := a.Orphans()
	must(t, err)
	if !o[tk.ID] {
		t.Fatal("not orphaned")
	}
	if n, _ := a.activeWorkers(); n != 0 {
		t.Fatalf("an orphan counts against the limit: active = %d", n)
	}
	// saddle resume brings it back.
	r, err := a.Resume(tk.ID)
	must(t, err)
	if r.Task != tk.ID {
		t.Fatalf("resume = %+v", r)
	}
	if o, _ := a.Orphans(); o[tk.ID] {
		t.Fatal("still orphaned after resume")
	}
	if _, err := a.Resume(tk.ID); err == nil {
		t.Fatal("resumed a task whose window is alive")
	}
}

// saddle rescue snapshots uncommitted files to rescue/<task> and a diff
// under .saddle/rescue/, then kills the task, releasing its claims.
func TestRescueSnapshotsAndKills(t *testing.T) {
	a, ft := setup(t)
	tk, err := a.Spawn(SpawnReq{Title: "one", Claims: []string{"one/**"}})
	must(t, err)
	write(t, tk.Worktree, "one/a.txt", "committed\n")
	commitAll(t, tk.Worktree, "a")
	write(t, tk.Worktree, "one/b.txt", "uncommitted\n")
	serverDies(ft)

	r, err := a.Rescue(tk.ID)
	must(t, err)
	if r.Branch != "rescue/"+tk.ID {
		t.Fatalf("branch = %q", r.Branch)
	}
	for f, want := range map[string]string{"one/a.txt": "committed", "one/b.txt": "uncommitted"} {
		if out := git(t, a.Root, "show", r.Branch+":"+f); out != want {
			t.Errorf("%s on %s = %q", f, r.Branch, out)
		}
	}
	d, err := os.ReadFile(r.Diff)
	must(t, err)
	if !strings.HasPrefix(r.Diff, a.stateDir("rescue")) || !strings.Contains(string(d), "+uncommitted") {
		t.Fatalf("diff %s:\n%s", r.Diff, d)
	}
	got, _ := a.Store.Task(tk.ID)
	if got.Status != store.Killed || !strings.Contains(got.Summary, "rescue/"+tk.ID) {
		t.Fatalf("after rescue: status %s summary %q", got.Status, got.Summary)
	}
	if cl, _ := a.Store.Claims(); len(cl[tk.ID]) != 0 {
		t.Fatalf("claims kept: %v", cl[tk.ID])
	}
	// The diff restores the files onto a clean checkout of the branch.
	write(t, tk.Worktree, "one/b.txt", "")
	must(t, os.Remove(filepath.Join(tk.Worktree, "one/b.txt")))
	git(t, tk.Worktree, "apply", r.Diff)
	if b, _ := os.ReadFile(filepath.Join(tk.Worktree, "one/b.txt")); string(b) != "uncommitted\n" {
		t.Fatalf("restored b.txt = %q", b)
	}
}
