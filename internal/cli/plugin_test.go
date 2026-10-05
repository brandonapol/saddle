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
	"github.com/brandonapol/saddle/internal/doctor"
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
	_ = a.Store.Notify(app.OrchestratorID, store.NoticeInfo, "t1 landed") // the queue as delivered; the policy would digest it (#222)
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
	_ = a.Store.Notify(app.OrchestratorID, store.NoticeInfo, "t1 spawned")
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
	for _, name := range []string{"wait", "setup", "install"} {
		cmd, _, err := Root().Find([]string{"plugin", name})
		if err != nil || cmd.Name() != name {
			t.Fatalf("saddle plugin %s: %v", name, err)
		}
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

// fakeDoctor returns rs and counts its runs.
type fakeDoctor struct {
	rs   []doctor.Result
	runs int
}

func (f *fakeDoctor) run(string) []doctor.Result {
	f.runs++
	return f.rs
}

func marker(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, ".saddle", onboardMarker))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func TestOnboardInitsAndRunsDoctorOnceOnUninitializedRepo(t *testing.T) {
	root := bareRepo(t)
	doc := &fakeDoctor{rs: []doctor.Result{
		{Name: "tmux", Status: doctor.OK, Detail: "tmux 3.4"},
		{Name: "branch protection", Status: doctor.Warn, Detail: "none", Fix: "protect main"},
	}}
	var out bytes.Buffer
	if !onboard(&out, root, doc.run) {
		t.Fatalf("warnings blocked the plugin:\n%s", out.String())
	}
	got := out.String()
	for _, want := range []string{"Howdy", "saddle init", "branch protection", "protect main"} {
		if !strings.Contains(got, want) {
			t.Errorf("first use missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\x1b") {
		t.Errorf("plugin output is captured by Claude Code; it must not be colored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".saddle", "config.toml")); err != nil {
		t.Fatalf("not initialized: %v", err)
	}
	if m := marker(t, root); m != "ok" {
		t.Fatalf("marker %q, want ok", m)
	}

	out.Reset()
	if !onboard(&out, filepath.Join(root), doc.run) || doc.runs != 1 || out.Len() != 0 {
		t.Fatalf("second use: runs %d, out %q", doc.runs, out.String())
	}
}

func TestOnboardSkipsInitializedRepo(t *testing.T) {
	a := pluginRepo(t)
	doc := &fakeDoctor{}
	var out bytes.Buffer
	if !onboard(&out, a.Root, doc.run) || doc.runs != 0 || out.Len() != 0 {
		t.Fatalf("already initialized: runs %d, out %q", doc.runs, out.String())
	}
	if m := marker(t, a.Root); m != "" {
		t.Fatalf("marker %q written for a repo the plugin didn't set up", m)
	}
}

func TestOnboardFailingDoctorBlocksUntilFixed(t *testing.T) {
	root := bareRepo(t)
	doc := &fakeDoctor{rs: []doctor.Result{{Name: "gh auth", Status: doctor.Fail, Detail: "not logged in", Fix: "gh auth login"}}}
	var out bytes.Buffer
	if onboard(&out, root, doc.run) {
		t.Fatalf("a failing check didn't block:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "FAIL") || !strings.Contains(out.String(), "gh auth login") || !strings.Contains(out.String(), "again") {
		t.Fatalf("blocked output:\n%s", out.String())
	}
	if m := marker(t, root); m != "pending" {
		t.Fatalf("marker %q, want pending", m)
	}

	// Next use reruns only the doctor: no second banner or init.
	doc.rs = []doctor.Result{{Name: "gh auth", Status: doctor.OK, Detail: "logged in"}}
	out.Reset()
	if !onboard(&out, root, doc.run) || doc.runs != 2 {
		t.Fatalf("fixed: runs %d out %q", doc.runs, out.String())
	}
	if strings.Contains(out.String(), "Howdy") {
		t.Fatalf("banner shown twice:\n%s", out.String())
	}
	if m := marker(t, root); m != "ok" {
		t.Fatalf("marker %q, want ok", m)
	}
	out.Reset()
	if !onboard(&out, root, doc.run) || doc.runs != 2 || out.Len() != 0 {
		t.Fatalf("third use: runs %d out %q", doc.runs, out.String())
	}
}

func TestOnboardLeavesAgentsAndNonRepos(t *testing.T) {
	doc := &fakeDoctor{}
	dir := t.TempDir()
	t.Setenv("SADDLE_ROOT", "")
	t.Setenv("SADDLE_TASK", "")
	var out bytes.Buffer
	if !onboard(&out, dir, doc.run) || doc.runs != 0 {
		t.Fatalf("non-repo: runs %d out %q", doc.runs, out.String())
	}
	root := bareRepo(t)
	t.Setenv("SADDLE_TASK", "t1")
	if !onboard(&out, root, doc.run) || doc.runs != 0 {
		t.Fatalf("agent: runs %d out %q", doc.runs, out.String())
	}
	for _, d := range []string{dir, root} {
		if _, err := os.Stat(filepath.Join(d, ".saddle")); !os.IsNotExist(err) {
			t.Fatalf(".saddle created in %s: %v", d, err)
		}
	}
}

// fakeExec stands in for PATH and running programs in install checks.
type fakeExec struct {
	path    map[string]bool
	version string
	ran     [][]string
	out     *bytes.Buffer
	seen    []string // what out held when each program ran
	onRun   func(args []string)
}

func (f *fakeExec) deps() installDeps {
	return installDeps{
		lookPath: func(name string) (string, error) {
			if f.path[name] {
				return "/bin/" + name, nil
			}
			return "", exec.ErrNotFound
		},
		run: func(name string, args ...string) (string, error) {
			all := append([]string{name}, args...)
			if name == "saddle" && len(args) == 1 && args[0] == "version" {
				return f.version, nil
			}
			f.ran = append(f.ran, all)
			if f.out != nil {
				f.seen = append(f.seen, f.out.String())
			}
			if f.onRun != nil {
				f.onRun(all)
			}
			return "", nil
		},
	}
}

func TestCheckInstallStates(t *testing.T) {
	for _, tc := range []struct {
		name, have string
		onPath     bool
		want       installState
	}{
		{"missing", "", false, installMissing},
		{"outdated", "v0.0.9", true, installOutdated},
		{"outdated minor", "v0.1.0", true, installOutdated}, // want is 0.2.0 below
		{"current", "v0.2.0", true, installCurrent},
		{"current ahead of tag", "v0.2.0-3-gabc1234-dirty", true, installCurrent},
		{"newer", "v1.0.0", true, installCurrent},
		{"dev build", "dev", true, installUnknown},
		{"commit build", "abc1234", true, installUnknown},
		{"pseudo-version", "v0.0.0-20261003120000-abcdef123456", true, installUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeExec{path: map[string]bool{"saddle": tc.onPath}, version: tc.have}
			got := checkInstall(f.deps(), "0.2.0")
			if got.State != tc.want {
				t.Fatalf("have %q: state %v, want %v", tc.have, got.State, tc.want)
			}
		})
	}
}

func TestInstallPrintsPlanAndNeverRunsWithoutYes(t *testing.T) {
	var out bytes.Buffer
	f := &fakeExec{path: map[string]bool{"saddle": true, "go": true}, version: "v0.0.9"}
	if ok := runInstall(&out, f.deps(), "0.1.0", false); ok {
		t.Fatal("outdated binary reported ok")
	}
	if len(f.ran) != 0 {
		t.Fatalf("ran %v without --yes", f.ran)
	}
	for _, want := range []string{"v0.0.9", "0.1.0", "go install github.com/brandonapol/saddle/cmd/saddle@v0.1.0", "--yes"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("plan missing %q:\n%s", want, out.String())
		}
	}
}

func TestInstallYesPrintsCommandBeforeRunningIt(t *testing.T) {
	var out bytes.Buffer
	f := &fakeExec{path: map[string]bool{"go": true}, out: &out}
	f.onRun = func([]string) { f.path["saddle"] = true; f.version = "v0.1.0" }
	if ok := runInstall(&out, f.deps(), "0.1.0", true); !ok {
		t.Fatalf("install failed:\n%s", out.String())
	}
	want := []string{"go", "install", "github.com/brandonapol/saddle/cmd/saddle@v0.1.0"}
	if len(f.ran) != 1 || strings.Join(f.ran[0], " ") != strings.Join(want, " ") {
		t.Fatalf("ran %v, want %v", f.ran, want)
	}
	if !strings.Contains(f.seen[0], "+ go install github.com/brandonapol/saddle/cmd/saddle@v0.1.0") {
		t.Fatalf("command not printed before it ran; output then:\n%s", f.seen[0])
	}
	if !strings.Contains(out.String(), "v0.1.0 installed") {
		t.Fatalf("no confirmation:\n%s", out.String())
	}
}

func TestInstallWithoutGoGivesInstructions(t *testing.T) {
	var out bytes.Buffer
	f := &fakeExec{path: map[string]bool{}}
	if runInstall(&out, f.deps(), "0.1.0", true) || len(f.ran) != 0 {
		t.Fatalf("ran %v with no Go installed", f.ran)
	}
	for _, want := range []string{"not on your PATH", "go.dev/dl", "QUICKSTART"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("instructions missing %q:\n%s", want, out.String())
		}
	}
}

func TestInstallCurrentAndDevAreQuiet(t *testing.T) {
	for _, have := range []string{"v0.1.0", "dev"} {
		var out bytes.Buffer
		f := &fakeExec{path: map[string]bool{"saddle": true, "go": true}, version: have}
		if !runInstall(&out, f.deps(), "0.1.0", true) || out.Len() != 0 || len(f.ran) != 0 {
			t.Fatalf("have %s: out %q ran %v", have, out.String(), f.ran)
		}
	}
}

func TestPluginVersionMatchesManifest(t *testing.T) {
	v, err := pluginVersion(filepath.Join("..", "..", "plugin"))
	if err != nil || v == "" {
		t.Fatalf("plugin version %q: %v", v, err)
	}
	if _, _, _, ok := semver(v); !ok {
		t.Fatalf("plugin version %q isn't X.Y.Z", v)
	}
}
