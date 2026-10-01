package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/refguard"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/modelcontextprotocol/go-sdk/mcp"
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
	_ = os.Setenv(app.TestRefguardEnv, "1")
	os.Exit(m.Run())
}

type fakeTmux struct {
	session bool
	windows map[string]bool
	n       int
}

func (f *fakeTmux) HasSession() bool { return f.session }
func (f *fakeTmux) NewSession(name, dir, cmd string) (string, error) {
	f.session = true
	return f.NewWindow(name, dir, cmd)
}
func (f *fakeTmux) NewWindow(string, string, string) (string, error) {
	f.n++
	id := fmt.Sprintf("@%d", f.n)
	f.windows[id] = true
	return id, nil
}
func (f *fakeTmux) WindowName(string) (string, error)   { return "", nil }
func (f *fakeTmux) KillWindow(id string) error          { delete(f.windows, id); return nil }
func (f *fakeTmux) Alive(id string) bool                { return f.windows[id] }
func (f *fakeTmux) SendText(string, string) error       { return nil }
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

func write(t *testing.T, dir, rel, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stackSetup gives a repo with a bare origin, where t1 has landed a change to
// README.md, and a clone of origin to move main from.
func stackSetup(t *testing.T) (a *app.App, other string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SADDLE_ROOT", "")
	t.Setenv("SADDLE_TRAIN", "") // the ref guard Init installs reads these
	t.Setenv("SADDLE_TASK", "")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}
	root := t.TempDir()
	git(t, root, "init", "-q", "-b", "main")
	write(t, root, "README.md", "hi\n")
	git(t, root, "add", "-A")
	git(t, root, "commit", "-qm", "init")
	origin := t.TempDir()
	git(t, origin, "init", "-q", "--bare", "-b", "main")
	git(t, root, "remote", "add", "origin", origin)
	git(t, root, "push", "-q", "origin", "main")

	a, err := app.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	a.Tmux = &fakeTmux{windows: map[string]bool{}}
	a.Cfg.Test.Cmd = "true"
	tk, err := a.Spawn(app.SpawnReq{ID: "t1", Title: "readme"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, tk.Worktree, "README.md", "hi from t1\n")
	t.Setenv("SADDLE_TASK", tk.ID) // as t1's agent, so the ref guard lets it move its branch
	git(t, tk.Worktree, "commit", "-qam", "readme")
	t.Setenv("SADDLE_TASK", "")
	if err := a.Done(tk.ID, "readme"); err != nil {
		t.Fatal(err)
	}
	rs, err := a.Land()
	if err != nil || len(rs) != 1 || rs[0].State != store.TrainOK {
		t.Fatalf("land = %+v, %v", rs, err)
	}

	other = filepath.Join(t.TempDir(), "other")
	git(t, root, "clone", "-q", origin, other)
	return a, other
}

func callRestack(t *testing.T, a *app.App) (RestackOut, *mcp.CallToolResult) {
	t.Helper()
	ctx := context.Background()
	st, ct := mcp.NewInMemoryTransports()
	if _, err := New(a, app.OrchestratorID).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "restack"})
	if err != nil {
		t.Fatal(err)
	}
	var out RestackOut
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, &out); err != nil {
			t.Fatal(err)
		}
	}
	return out, res
}

func TestRestackToolMovesStack(t *testing.T) {
	a, other := stackSetup(t)
	write(t, other, "NEWS.md", "base moved\n")
	git(t, other, "add", "-A")
	git(t, other, "commit", "-qm", "news")
	git(t, other, "push", "-q", "origin", "main")
	head := git(t, other, "rev-parse", "HEAD")

	out, res := callRestack(t, a)
	if res.IsError {
		t.Fatalf("restack failed: %+v", res.Content)
	}
	if out.Base != head || out.Conflict != nil {
		t.Fatalf("out = %+v, want base %s", out, head)
	}
	moved := map[string]bool{}
	for _, m := range out.Moves {
		moved[m.Ref] = true
	}
	if !moved["refs/heads/saddle/t1-readme"] || !moved["refs/heads/"+a.Cfg.Integration] {
		t.Fatalf("moves = %+v", out.Moves)
	}
}

func TestRestackToolReportsConflictOwner(t *testing.T) {
	a, other := stackSetup(t)
	write(t, other, "README.md", "hi from main\n")
	git(t, other, "commit", "-qam", "readme on main")
	git(t, other, "push", "-q", "origin", "main")
	integ := git(t, a.Root, "rev-parse", a.Cfg.Integration)

	out, res := callRestack(t, a)
	if res.IsError {
		t.Fatalf("restack errored instead of reporting the conflict: %+v", res.Content)
	}
	c := out.Conflict
	if c == nil || c.Task != "t1" || len(c.Files) != 1 || c.Files[0] != "README.md" || !strings.Contains(out.Message, "t1") {
		t.Fatalf("out = %+v", out)
	}
	if len(out.Moves) != 0 || git(t, a.Root, "rev-parse", a.Cfg.Integration) != integ {
		t.Fatal("restack moved refs despite the conflict")
	}
}

func TestStatusWarnsOnDrift(t *testing.T) {
	a, _ := stackSetup(t)
	tk, err := a.Store.Task("t1")
	if err != nil {
		t.Fatal(err)
	}
	landed := git(t, a.Root, "rev-parse", tk.Branch)
	t.Setenv("SADDLE_TASK", "t1") // t1 rewrites its own landed branch
	git(t, a.Root, "update-ref", "refs/heads/"+tk.Branch, landed+"~")
	t.Setenv("SADDLE_TASK", "")
	drifted := git(t, a.Root, "rev-parse", tk.Branch)

	st, err := Status(a)
	if err != nil {
		t.Fatal(err)
	}
	want := "t1: landed " + landed[:12] + ", branch " + drifted[:12]
	if !slices.Contains(st.Warnings, want) {
		t.Fatalf("warnings = %q, want %q", st.Warnings, want)
	}
}
