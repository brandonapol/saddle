package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/lintgate"
	"github.com/brandonapol/saddle/internal/refguard"
	"github.com/brandonapol/saddle/internal/store"
)

// TestMain lets the test binary stand in for saddle, so Init installs the ref
// guard hook, which runs `<bin> refguard <state>`.
func TestMain(m *testing.M) {
	if len(os.Args) == 3 && os.Args[1] == "refguard" {
		if err := refguard.Hook(os.Args[2], os.Stdin, os.Getenv); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	// The hook runs this binary; built with -race it would sleep a second on
	// every exit, and so on every ref update.
	_ = os.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	_ = os.Setenv(TestRefguardEnv, "1")
	os.Exit(m.Run())
}

type fakeTmux struct {
	mu      sync.Mutex // SendWhenIdle retries and delivery checks run in goroutines
	frozen  bool       // SendText leaves the screen as it was
	session bool
	windows map[string]bool
	sent    map[string][]string
	names   map[string]string
	screens map[string]string // what Capture returns per window
	n       int
}

func (f *fakeTmux) HasSession() bool { return f.session }
func (f *fakeTmux) NewSession(name, dir, cmd string) (string, error) {
	f.session = true
	return f.NewWindow(name, dir, cmd)
}
func (f *fakeTmux) NewWindow(name, _, _ string) (string, error) {
	f.n++
	id := fmt.Sprintf("@%d", f.n)
	f.windows[id] = true
	f.names[id] = name
	return id, nil
}
func (f *fakeTmux) WindowName(id string) (string, error) { return f.names[id], nil }
func (f *fakeTmux) KillWindow(id string) error           { delete(f.windows, id); return nil }
func (f *fakeTmux) Alive(id string) bool                 { return f.windows[id] }

// SendText records text and, unless frozen, echoes it onto the screen the
// way a submitted prompt changes the pane.
func (f *fakeTmux) SendText(id, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent[id] = append(f.sent[id], text)
	if !f.frozen {
		if f.screens == nil {
			f.screens = map[string]string{}
		}
		f.screens[id] += "\n" + text
	}
	return nil
}
func (f *fakeTmux) Capture(id string, _ int) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.screens[id], nil
}
func (f *fakeTmux) setScreen(id, s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.screens == nil {
		f.screens = map[string]string{}
	}
	f.screens[id] = s
}
func (f *fakeTmux) sentTo(id string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent[id]...)
}
func (f *fakeTmux) SendKeys(string, ...string) error { return nil }
func (f *fakeTmux) KillSession() error               { return nil }

// git runs git in dir. In a task's worktree it runs as that task, the way
// its agent would, so the ref guard lets it move the task's branch.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = os.Environ()
	if rel, err := filepath.Rel(filepath.Join(filepath.Dir(filepath.Dir(dir)), "worktrees"), dir); err == nil && filepath.Base(filepath.Dir(dir)) == "worktrees" {
		id, _, _ := strings.Cut(rel, "-")
		cmd.Env = append(cmd.Env, "SADDLE_TASK="+id)
	}
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, ee.Stderr)
		}
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
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
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-qm", msg)
}

func setup(t *testing.T) (*App, *fakeTmux) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SADDLE_ROOT", "")
	// Tests act as nobody in particular unless they say otherwise; the ref
	// guard Init installs reads these.
	t.Setenv("SADDLE_TASK", "")
	t.Setenv("SADDLE_TRAIN", "")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	write(t, root, "billing/meter.go", "package billing\n\nfunc Meter() {}\n")
	write(t, root, "billing/invoice.go", "package billing\n\nfunc Invoice() int { return 1 }\n")
	write(t, root, "README.md", "hi\n")
	commitAll(t, root, "init")
	a, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	a.Cfg.Test.Cmd = "true"
	ft := &fakeTmux{windows: map[string]bool{}, sent: map[string][]string{}, names: map[string]string{}}
	a.Tmux = ft
	a.Cfg.CloseOnLand = true
	return a, ft
}

func TestInitInstallsRefGuard(t *testing.T) {
	a, _ := setup(t)
	must(t, a.Init())
	must(t, a.ensureIntegration())
	integ := "refs/heads/" + a.Cfg.Integration
	before := git(t, a.Root, "rev-parse", integ)
	git(t, a.Root, "commit", "-q", "--allow-empty", "-m", "next")
	head := git(t, a.Root, "rev-parse", "HEAD")

	updateRef := func(env ...string) error {
		cmd := exec.Command("git", "-C", a.Root, "update-ref", integ, head)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	}
	if err := updateRef("SADDLE_TASK=t2"); err == nil {
		t.Fatal("t2 moved the integration branch")
	}
	if got := git(t, a.Root, "rev-parse", integ); got != before {
		t.Fatalf("denied update moved integration to %s", got)
	}
	evs, err := a.Store.Events(100)
	must(t, err)
	denied := false
	for _, e := range evs {
		denied = denied || (e.Kind == refguard.KindDenied && e.Task == "t2")
	}
	if !denied {
		t.Fatalf("no %s event for t2: %v", refguard.KindDenied, evs)
	}
	must(t, updateRef("SADDLE_TRAIN=1", "SADDLE_TASK=t0"))
	if got := git(t, a.Root, "rev-parse", integ); got != head {
		t.Fatalf("train update left integration at %s, want %s", got, head)
	}
}

// Init chains a repo's own hook instead of refusing or clobbering it (#212).
func TestInitChainsForeignRefHook(t *testing.T) {
	a, _ := setup(t)
	hooks := git(t, a.Root, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	foreign := "#!/bin/sh\nexit 0\n"
	write(t, hooks, "reference-transaction", foreign)
	must(t, a.Init())
	b, err := os.ReadFile(filepath.Join(hooks, "reference-transaction"+lintgate.ChainSuffix))
	if err != nil || string(b) != foreign {
		t.Fatalf("repo hook not kept for chaining: %q, %v", b, err)
	}
}

func TestWarningsReportDrift(t *testing.T) {
	a := trainSetup(t)
	t1 := landTask(t, a, "t1", "one", map[string]string{"one.txt": "one\n"})
	landed := git(t, a.Root, "rev-parse", t1.Branch)
	if ws := a.Warnings(); len(ws) != 0 {
		t.Fatalf("warnings before drift: %v", ws)
	}

	// t1 rewrites its own branch after the train recorded it.
	cmd := exec.Command("git", "-C", a.Root, "update-ref", "refs/heads/"+t1.Branch, landed+"~")
	cmd.Env = append(os.Environ(), "SADDLE_TASK=t1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	drifted := git(t, a.Root, "rev-parse", t1.Branch)
	want := "t1: landed " + landed[:12] + ", branch " + drifted[:12]
	if ws := a.Warnings(); len(ws) != 1 || ws[0] != want {
		t.Fatalf("warnings = %q, want [%q]", ws, want)
	}
}

func TestSpawnClaimsAndWrites(t *testing.T) {
	a, _ := setup(t)
	t1, err := a.Spawn(SpawnReq{Title: "meter", Claims: []string{"billing/meter.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(t1.Worktree, "billing/meter.go")); err != nil {
		t.Fatalf("worktree not created: %v", err)
	}
	if _, err := a.Spawn(SpawnReq{Title: "dup", Claims: []string{"billing/**"}}); err == nil || !strings.Contains(err.Error(), "claim conflict") {
		t.Fatalf("overlapping spawn: err = %v", err)
	}
	t2, err := a.Spawn(SpawnReq{Title: "invoice"})
	if err != nil {
		t.Fatal(err)
	}
	// t2 may not touch t1's file, in its own worktree or anyone else's.
	if d := a.CheckWrite(t2.ID, filepath.Join(t2.Worktree, "billing/meter.go")); d.Allow || !strings.Contains(d.Reason, t1.ID) {
		t.Fatalf("cross-claim write allowed: %+v", d)
	}
	if d := a.CheckWrite(t2.ID, filepath.Join(t1.Worktree, "billing/invoice.go")); d.Allow || !strings.Contains(d.Reason, "outside your worktree") {
		t.Fatalf("write into another worktree allowed: %+v", d)
	}
	// First write auto-claims; then t1 is locked out of it.
	if d := a.CheckWrite(t2.ID, filepath.Join(t2.Worktree, "billing/invoice.go")); !d.Allow {
		t.Fatalf("unowned write denied: %+v", d)
	}
	if d := a.CheckWrite(t1.ID, filepath.Join(t1.Worktree, "billing/invoice.go")); d.Allow {
		t.Fatal("auto-claimed file not protected")
	}
	a.Cfg.Serial = []string{"go.sum"}
	if d := a.CheckWrite(t1.ID, filepath.Join(t1.Worktree, "go.sum")); d.Allow {
		t.Fatal("serial file write allowed")
	}
}

// A barrier task moves billing/ to pkg/billing/ while another task adds a new
// file under billing/. After both land, the new file must follow the move.
func TestLandFollowsDirectoryMove(t *testing.T) {
	a, ft := setup(t)
	mover, err := a.Spawn(SpawnReq{Title: "move billing", Claims: []string{"billing/**", "pkg/billing/**"}})
	if err != nil {
		t.Fatal(err)
	}
	adder, err := a.Spawn(SpawnReq{Title: "add tax", Claims: []string{"tax/**"}})
	if err != nil {
		t.Fatal(err)
	}
	git(t, mover.Worktree, "mv", "billing", "pkg-tmp")
	must(t, os.MkdirAll(filepath.Join(mover.Worktree, "pkg"), 0o755))
	git(t, mover.Worktree, "mv", "pkg-tmp", "pkg/billing")
	commitAll(t, mover.Worktree, "move billing")

	// adder writes a new file under the old directory (not claimed by it, but in the real flow
	// the hook would deny this; here we exercise git's rename following directly).
	write(t, adder.Worktree, "billing/tax.go", "package billing\n\nfunc Tax() {}\n")
	write(t, adder.Worktree, "tax/rates.go", "package tax\n")
	commitAll(t, adder.Worktree, "add tax")

	if err := a.Done(mover.ID, "moved billing"); err != nil {
		t.Fatal(err)
	}
	if err := a.Done(adder.ID, "added tax"); err != nil {
		t.Fatal(err)
	}
	rs, err := a.Land()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.State != store.TrainOK {
			t.Fatalf("land %s: %s %s", r.Task, r.State, r.Note)
		}
	}
	files := git(t, a.Root, "ls-tree", "-r", "--name-only", a.Cfg.Integration)
	lines := "\n" + files + "\n"
	if !strings.Contains(lines, "\npkg/billing/tax.go\n") || strings.Contains(lines, "\nbilling/") {
		t.Fatalf("new file did not follow the move:\n%s", files)
	}
	if ft.Alive(mover.Window) {
		t.Fatal("landed task window not closed")
	}
	if _, err := os.Stat(mover.Worktree); !os.IsNotExist(err) {
		t.Fatal("landed worktree not removed")
	}
	// The orchestrator heard about both.
	ns, _ := a.Store.TakeNotices(OrchestratorID, false)
	if len(ns) < 2 {
		t.Fatalf("orchestrator notices = %d", len(ns))
	}
}

func TestLandBroadcastsRenamesAndRemapsClaims(t *testing.T) {
	a, _ := setup(t)
	mover, _ := a.Spawn(SpawnReq{Title: "move", Claims: []string{"README.md", "docs/**"}})
	other, _ := a.Spawn(SpawnReq{Title: "meter work", Claims: []string{"billing/meter.go"}})
	must(t, os.MkdirAll(filepath.Join(mover.Worktree, "docs"), 0o755))
	git(t, mover.Worktree, "mv", "README.md", "docs/README.md")
	commitAll(t, mover.Worktree, "move readme")
	must(t, a.Done(mover.ID, "moved"))
	if _, err := a.Land(); err != nil {
		t.Fatal(err)
	}
	ns, _ := a.Store.TakeNotices(other.ID, false)
	if len(ns) != 1 || !strings.Contains(ns[0].Text, "README.md → docs/README.md") {
		t.Fatalf("rename notice = %+v", ns)
	}
}

func TestConflictReturnsToProducer(t *testing.T) {
	a, ft := setup(t)
	t1, _ := a.Spawn(SpawnReq{Title: "one"})
	t2, _ := a.Spawn(SpawnReq{Title: "two"})
	write(t, t1.Worktree, "README.md", "one\n")
	commitAll(t, t1.Worktree, "one")
	write(t, t2.Worktree, "README.md", "two\n")
	commitAll(t, t2.Worktree, "two")
	must(t, a.Done(t1.ID, "one"))
	must(t, a.Done(t2.ID, "two"))
	rs, err := a.Land()
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) != 2 || rs[0].State != store.TrainOK || rs[1].State != store.TrainError {
		t.Fatalf("results = %+v", rs)
	}
	got, _ := a.Store.Task(t2.ID)
	if got.Status != store.Conflict {
		t.Fatalf("t2 status = %s", got.Status)
	}
	if len(ft.sent[t2.Window]) == 0 {
		t.Fatal("idle producer was not woken")
	}
	ns, _ := a.Store.TakeNotices(t2.ID, true)
	if len(ns) == 0 || !strings.Contains(ns[len(ns)-1].Text, "README.md") {
		t.Fatalf("conflict notice = %+v", ns)
	}
	// The worktree was left clean, so the agent can sync and resolve.
	if d, _ := gitx.Dirty(t2.Worktree); len(d) > 0 {
		t.Fatalf("worktree left dirty: %v", d)
	}
	rr, err := a.Sync(t2.ID)
	if err != nil || rr.OK || len(rr.Conflicts) != 1 {
		t.Fatalf("sync = %+v, %v", rr, err)
	}
}

// The orchestrator repairs stacks through tasks and restack, never by hand;
// workers never move integration.
func TestBriefsCarryStackRules(t *testing.T) {
	a, _ := setup(t)
	orch := a.orchestratorBrief()
	for _, want := range []string{
		"Stack or base problems (base moved, CI failing on a stacked PR, drift)",
		"find the owning task and `message` it, or call `restack` if the base moved",
		"Restack first; if that fails, see Getting unstuck.",
	} {
		if !strings.Contains(orch, want) {
			t.Errorf("orchestrator brief lacks %q", want)
		}
	}
	w := a.workerBrief(store.Task{ID: "t1", Title: "x", Branch: "saddle/t1-x"}, nil)
	if want := "Only the merge train pushes or moves " + a.Cfg.Integration + "."; !strings.Contains(w, want) {
		t.Errorf("worker brief lacks %q", want)
	}
}

// A spawn whose every claim covers files landed or queued tasks changed owns
// no new work: it is likely a stack repair, so it needs confirmation first.
func TestSpawnNeedsConfirmWhenClaimsOnlyCoverTrainWork(t *testing.T) {
	a := trainSetup(t)
	landTask(t, a, "t1", "meter", map[string]string{"billing/meter.go": "package billing\n\nfunc Meter() int { return 2 }\n"})

	noTrace := func(id, name string) {
		t.Helper()
		if _, err := a.Store.Task(id); err == nil {
			t.Fatalf("%s: task row created before confirmation", id)
		}
		if _, err := os.Stat(a.stateDir("worktrees", name)); !os.IsNotExist(err) {
			t.Fatalf("%s: worktree created before confirmation", id)
		}
		if gitx.BranchExists(a.Root, "saddle/"+name) {
			t.Fatalf("%s: branch created before confirmation", id)
		}
	}
	_, err := a.Spawn(SpawnReq{ID: "t2", Title: "fix meter PR", Claims: []string{"billing/**"}})
	if !errors.Is(err, ErrNeedsConfirm) || !strings.Contains(err.Error(), "t1") {
		t.Fatalf("spawn over landed work: err = %v", err)
	}
	noTrace("t2", "t2-fix-meter-pr")

	// Work queued in the train counts too, even when its task holds no claims.
	q, err := a.Spawn(SpawnReq{ID: "t3", Title: "readme"})
	must(t, err)
	write(t, q.Worktree, "README.md", "queued\n")
	commitAll(t, q.Worktree, "readme")
	must(t, a.Done(q.ID, "readme"))
	_, err = a.Spawn(SpawnReq{ID: "t4", Title: "fix readme PR", Claims: []string{"README.md", "billing/meter.go"}})
	if !errors.Is(err, ErrNeedsConfirm) || !strings.Contains(err.Error(), "t3") {
		t.Fatalf("spawn over queued work: err = %v", err)
	}
	noTrace("t4", "t4-fix-readme-pr")

	// Confirmed, it goes ahead.
	if _, err := a.Spawn(SpawnReq{ID: "t4", Title: "fix readme PR", Claims: []string{"billing/meter.go"}, Confirm: true}); err != nil {
		t.Fatalf("confirmed spawn: %v", err)
	}
	// A claim on new ground makes it a normal spawn.
	if _, err := a.Spawn(SpawnReq{ID: "t5", Title: "tax", Claims: []string{"README.md", "tax/**"}}); err != nil {
		t.Fatalf("normal spawn: %v", err)
	}
}

// A race-built test binary sleeps a second on exit unless GORACE says
// otherwise. Every ref update runs it as the ref guard, so that second, paid
// on each one, pushed the race run of this package past go test's timeout.
func TestRefguardHookExitsPromptly(t *testing.T) {
	cmd := exec.Command(os.Args[0], "refguard", "prepared")
	cmd.Dir = t.TempDir()
	start := time.Now()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("refguard hook: %v: %s", err, out)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("refguard hook took %v; GORACE atexit_sleep_ms not passed to it?", d)
	}
}
