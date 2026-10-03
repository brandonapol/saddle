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
	// The hook runs this binary; built with -race it would sleep a second on
	// every exit, and so on every ref update.
	_ = os.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
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
	// Claude's legacy top-level decision only takes approve/block; "deny"
	// there can fail its output validation, so Claude gets only hookSpecificOutput.
	if _, ok := out["decision"]; ok {
		t.Fatalf("claude deny carries a top-level decision: %v", out)
	}
	// Grok's PreToolUse payload is camelCase and names the path "path".
	// The deny has to be both Claude's permissionDecision and Grok's decision.
	out = run(t, a, t2.ID, map[string]any{
		"hookEventName": "pre_tool_use", "hook_event_name": "PreToolUse",
		"toolName": "write_file", "sessionId": "sid",
		"toolInput": map[string]any{"path": filepath.Join(t2.Worktree, "a.go")},
	})
	if out["decision"] != "deny" {
		t.Fatalf("grok deny = %v", out)
	}
	hs, _ = out["hookSpecificOutput"].(map[string]any)
	if hs["permissionDecision"] != "deny" || !strings.Contains(hs["permissionDecisionReason"].(string), t1.ID) {
		t.Fatalf("grok out = %v", out)
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

func TestOrchestratorHookDeliversNoticesWithoutTakingTheSession(t *testing.T) {
	a := setup(t)
	if _, err := a.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	if err := a.Notify(app.OrchestratorID, store.NoticeInfo, "t1 landed"); err != nil {
		t.Fatal(err)
	}
	out := HandleOrchestrator(a, Input{Event: "SessionStart", SessionID: "user-session"})
	if out == nil || !strings.Contains(out.Specific.AdditionalContext, "[saddle] t1 landed") {
		t.Fatalf("SessionStart: %+v", out)
	}
	// The TUI resumes t0's session id; it must never resume the user's own session.
	if t0, _ := a.Store.Task(app.OrchestratorID); t0.SessionID != "" {
		t.Fatalf("session id recorded: %q", t0.SessionID)
	}
	if out := HandleOrchestrator(a, Input{Event: "PostToolUse"}); out != nil {
		t.Fatalf("notice delivered twice: %+v", out)
	}
	if out := HandleOrchestrator(a, Input{Event: "PreToolUse", ToolInput: map[string]any{"file_path": filepath.Join(a.Root, "a.go")}}); out != nil {
		t.Fatalf("orchestrator writes are not claim-checked: %+v", out)
	}
}

func TestOrchestratorHookStopContinuesForActionNotices(t *testing.T) {
	a := setup(t)
	if _, err := a.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	_ = a.Notify(app.OrchestratorID, store.NoticeInfo, "t1 spawned")
	if out := HandleOrchestrator(a, Input{Event: "Stop"}); out != nil {
		t.Fatalf("info notice alone kept the session going: %+v", out)
	}
	_ = a.Notify(app.OrchestratorID, store.NoticeAction, "t2 conflicted")
	if out := HandleOrchestrator(a, Input{Event: "Stop", StopHookActive: true}); out != nil {
		t.Fatalf("blocked a stop that already continued once: %+v", out)
	}
	out := HandleOrchestrator(a, Input{Event: "Stop"})
	if out == nil || out.Decision != "block" || !strings.Contains(out.Reason, "t2 conflicted") {
		t.Fatalf("Stop: %+v", out)
	}
}
