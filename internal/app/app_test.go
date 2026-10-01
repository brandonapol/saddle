package app

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
)

type fakeTmux struct {
	session bool
	windows map[string]bool
	sent    map[string][]string
	names   map[string]string
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
func (f *fakeTmux) SendText(id, text string) error {
	f.sent[id] = append(f.sent[id], text)
	return nil
}
func (f *fakeTmux) Capture(string, int) (string, error) { return "", nil }
func (f *fakeTmux) SendKeys(string, ...string) error    { return nil }
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
