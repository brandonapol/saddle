package hook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/gitx"
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
	_ = os.Setenv(app.TestRefguardEnv, "1")
	os.Exit(m.Run())
}

type nopTmux struct{ n int }

func (f *nopTmux) HasSession() bool                                  { return true }
func (f *nopTmux) NewSession(string, string, string) (string, error) { return "@0", nil }
func (f *nopTmux) NewWindow(string, string, string) (string, error) {
	f.n++
	return "@" + string(rune('0'+f.n)), nil
}
func (f *nopTmux) KillWindow(string) error             { return nil }
func (f *nopTmux) Alive(string) bool                   { return false }
func (f *nopTmux) SendText(string, string) error       { return nil }
func (f *nopTmux) Capture(string, int) (string, error) { return "", nil }
func (f *nopTmux) SendKeys(string, ...string) error    { return nil }
func (f *nopTmux) KillSession() error                  { return nil }

func setup(t *testing.T) *app.App {
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
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-qm", "init"}} {
		if _, err := gitx.Run(root, args...); err != nil {
			t.Fatal(err)
		}
	}
	a, err := app.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	a.Tmux = &nopTmux{}
	t.Cleanup(func() { a.Close() })
	return a
}

func run(t *testing.T, a *app.App, task string, in map[string]any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(in)
	var out bytes.Buffer
	if err := Run(a, task, bytes.NewReader(b), &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestPreToolUseDeniesOtherTasksFiles(t *testing.T) {
	a := setup(t)
	t1, err := a.Spawn(app.SpawnReq{Title: "one", Claims: []string{"a.go"}})
	if err != nil {
		t.Fatal(err)
	}
	t2, _ := a.Spawn(app.SpawnReq{Title: "two"})
	out := run(t, a, t2.ID, map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": "Edit",
		"tool_input": map[string]any{"file_path": filepath.Join(t2.Worktree, "a.go")},
	})
	hs, _ := out["hookSpecificOutput"].(map[string]any)
	if hs["permissionDecision"] != "deny" || !strings.Contains(hs["permissionDecisionReason"].(string), t1.ID) {
		t.Fatalf("out = %v", out)
	}
	if out := run(t, a, t1.ID, map[string]any{
		"hook_event_name": "PreToolUse", "tool_name": "Write",
		"tool_input": map[string]any{"file_path": filepath.Join(t1.Worktree, "a.go")},
	}); out != nil {
		t.Fatalf("owner denied: %v", out)
	}
}

func TestNoticesDeliveredAndStopBlocks(t *testing.T) {
	a := setup(t)
	t1, _ := a.Spawn(app.SpawnReq{Title: "one"})
	if err := a.Store.Notify(t1.ID, store.NoticeInfo, "fyi"); err != nil {
		t.Fatal(err)
	}
	out := run(t, a, t1.ID, map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Read"})
	hs, _ := out["hookSpecificOutput"].(map[string]any)
	if !strings.Contains(hs["additionalContext"].(string), "[saddle] fyi") {
		t.Fatalf("post out = %v", out)
	}
	if err := a.Store.Notify(t1.ID, store.NoticeAction, "fix the conflict"); err != nil {
		t.Fatal(err)
	}
	out = run(t, a, t1.ID, map[string]any{"hook_event_name": "Stop"})
	if out["decision"] != "block" || !strings.Contains(out["reason"].(string), "fix the conflict") {
		t.Fatalf("stop out = %v", out)
	}
	// Delivered once: the next stop lets the agent go idle.
	if out := run(t, a, t1.ID, map[string]any{"hook_event_name": "Stop", "stop_hook_active": true}); out != nil {
		t.Fatalf("second stop = %v", out)
	}
	if got, _ := a.Store.Task(t1.ID); got.Status != store.Idle {
		t.Fatalf("status = %s", got.Status)
	}
}
