package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/banner"
	"github.com/brandonapol/saddle/internal/store"
)

// pluginRepo returns a git repo with saddle initialized and the orchestrator
// task created, as `saddle plugin brief` leaves it.
func pluginRepo(t *testing.T) *app.App {
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
	root := t.TempDir()
	gitRun(t, root, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-qm", "init")
	a, err := app.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	a.Tmux = &fakeTmux{}
	if _, err := a.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestOpenPluginStaysOutOfReposWithoutSaddle(t *testing.T) {
	t.Setenv("SADDLE_TASK", "")
	t.Setenv("SADDLE_ROOT", "")
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q", "-b", "main")
	a, why := openPlugin(dir)
	if a != nil || !strings.Contains(why, "saddle init") {
		t.Fatalf("app %v, why %q", a, why)
	}
	// The plugin runs in every project: it must not set saddle up by itself.
	if _, err := os.Stat(filepath.Join(dir, ".saddle")); !os.IsNotExist(err) {
		t.Fatalf(".saddle created in a repo that never ran saddle init: %v", err)
	}
}

func TestOpenPluginFindsRepoFromSubdirAndSkipsAgents(t *testing.T) {
	a := pluginRepo(t)
	sub := filepath.Join(a.Root, "deep", "dir")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	b, why := openPlugin(sub)
	if b == nil {
		t.Fatalf("subdir: %s", why)
	}
	b.Close()
	if b.Root != a.Root {
		t.Fatalf("root %q, want %q", b.Root, a.Root)
	}
	t.Setenv("SADDLE_TASK", "t3")
	if b, why := openPlugin(a.Root); b != nil || !strings.Contains(why, "saddle's own agents") {
		t.Fatalf("inside an agent: app %v, why %q", b, why)
	}
}

func hookIn(t *testing.T, dir, event string) *bytes.Buffer {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"hook_event_name": event, "cwd": dir, "session_id": "s"})
	return bytes.NewBuffer(b)
}

func TestPluginHookOnlyActsWhileEngineRuns(t *testing.T) {
	a := pluginRepo(t)
	if err := a.Notify(app.OrchestratorID, store.NoticeAction, "t1 needs you"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runPluginHook(hookIn(t, a.Root, "UserPromptSubmit"), &out); err != nil || out.Len() != 0 {
		t.Fatalf("no engine: out %q err %v", out.String(), err)
	}

	release, err := a.AcquireLock(app.LockUp)
	if err != nil {
		t.Fatal(err)
	}
	if err := runPluginHook(hookIn(t, a.Root, "UserPromptSubmit"), &out); err != nil || out.Len() != 0 {
		t.Fatalf("saddle up owns the orchestrator, but the hook took its notice: %q err %v", out.String(), err)
	}
	release()

	release, err = a.AcquireLock(app.LockEngine)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := runPluginHook(hookIn(t, a.Root, "UserPromptSubmit"), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "t1 needs you") {
		t.Fatalf("engine running: out %q", out.String())
	}

	t.Setenv("SADDLE_TASK", "t1")
	_ = a.Notify(app.OrchestratorID, store.NoticeAction, "t2 needs you")
	out.Reset()
	if err := runPluginHook(hookIn(t, a.Root, "UserPromptSubmit"), &out); err != nil || out.Len() != 0 {
		t.Fatalf("a worker session got the orchestrator's notice: %q err %v", out.String(), err)
	}
}

func TestWaitNoticesReturnsOnActionWithHeldInfo(t *testing.T) {
	a := pluginRepo(t)
	_ = a.Notify(app.OrchestratorID, store.NoticeInfo, "t1 landed")
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = a.Notify(app.OrchestratorID, store.NoticeAction, "t2 conflicted")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := waitNotices(ctx, a, 5*time.Millisecond, &out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"[saddle] t1 landed", "[saddle] t2 conflicted", "engine is not running"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	if n, _ := a.Store.PendingNotices(app.OrchestratorID); n != 0 {
		t.Fatalf("%d notices left pending", n)
	}
}

func TestWaitNoticesTimesOutCleanly(t *testing.T) {
	a := pluginRepo(t)
	_ = a.Notify(app.OrchestratorID, store.NoticeInfo, "t1 spawned")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	if err := waitNotices(ctx, a, 5*time.Millisecond, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No saddle events need you yet") {
		t.Fatalf("out %q", out.String())
	}
	if n, _ := a.Store.PendingNotices(app.OrchestratorID); n != 1 {
		t.Fatalf("info notice consumed on timeout: %d pending", n)
	}
}

func TestPluginBriefReportsEngineAndAgents(t *testing.T) {
	a := pluginRepo(t)
	if _, err := a.Spawn(app.SpawnReq{Title: "meter"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeBrief(&out, a); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Saddle orchestrator", "Engine: not running", "(meter): running"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("brief missing %q", want)
		}
	}
}

func TestPluginCommandRegistered(t *testing.T) {
	cmd, _, err := Root().Find([]string{"plugin", "wait"})
	if err != nil || cmd.Name() != "wait" {
		t.Fatalf("saddle plugin wait: %v", err)
	}
}

// bareRepo is a git repo with one commit where saddle init never ran.
func bareRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("SADDLE_ROOT", "")
	t.Setenv("SADDLE_TASK", "")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@example.com")
	root := t.TempDir()
	gitRun(t, root, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, root, "add", "-A")
	gitRun(t, root, "commit", "-qm", "init")
	return root
}

func runInit(t *testing.T, root string, tty bool, args ...string) string {
	t.Helper()
	old := stdoutIsTTY
	stdoutIsTTY = func(io.Writer) bool { return tty }
	t.Cleanup(func() { stdoutIsTTY = old })
	t.Chdir(root)
	var out bytes.Buffer
	cmd := Root()
	cmd.SetOut(&out)
	cmd.SetArgs(append([]string{"init"}, args...))
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestInitPrintsHowdyBannerOnTTY(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	root := bareRepo(t)
	out := runInit(t, root, true)
	if !strings.Contains(out, banner.Howdy()) || !strings.Contains(out, "initialized") {
		t.Fatalf("init on a TTY:\n%s", out)
	}
	if strings.Contains(out, "\x1b") {
		t.Fatalf("NO_COLOR set but init printed escapes: %q", out)
	}
}

func TestInitBannerSuppressedByQuietAndNonTTY(t *testing.T) {
	root := bareRepo(t)
	if out := runInit(t, root, true, "--quiet"); strings.Contains(out, "Howdy") {
		t.Fatalf("--quiet printed the banner:\n%s", out)
	}
	if out := runInit(t, root, false); strings.Contains(out, "Howdy") || !strings.Contains(out, "initialized") {
		t.Fatalf("non-TTY init:\n%s", out)
	}
}
