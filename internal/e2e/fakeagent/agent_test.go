package fakeagent

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// hookScript answers like saddle's hook: it logs each payload, denies writes
// and commands under blocked/, rewrites the Bash command "heavy" as saddle
// routes heavy runs through its queue, adds context to UserPromptSubmit and blocks the first Stop
// once with a notice.
const hookScript = `#!/bin/sh
in=$(cat)
printf '%s\n' "$in" >> "$LOGDIR/hooks.jsonl"
case "$in" in
  *'"PreToolUse"'*blocked/*) echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"[saddle] t9 owns blocked/"}}' ;;
  *'"PreToolUse"'*'"command":"heavy"'*) echo '{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"echo queued heavy > ran.txt"}}}' ;;
  *'"UserPromptSubmit"'*) echo '{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"[saddle] notice: rebase please"}}' ;;
  *'"Stop"'*) if [ ! -e "$LOGDIR/stopped" ]; then touch "$LOGDIR/stopped"; echo '{"decision":"block","reason":"[saddle] pending: land failed"}'; fi ;;
esac
`

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func sh(t *testing.T, dir, cmd string) string {
	t.Helper()
	c := exec.Command("sh", "-c", cmd)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", cmd, err, out)
	}
	return strings.TrimSpace(string(out))
}

type rig struct {
	t      *testing.T
	dir    string // scripts and logs
	work   string
	in     *io.PipeWriter
	out    *syncBuf
	agent  *Agent
	exited chan int
}

func newRig(t *testing.T, s Script) *rig {
	t.Helper()
	d := t.TempDir()
	r := &rig{t: t, dir: filepath.Join(d, "scripts"), work: filepath.Join(d, "work"), out: &syncBuf{}, exited: make(chan int, 1)}
	for _, p := range []string{r.dir, r.work} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	sh(t, r.work, "git init -q && git commit -q --allow-empty -m init")
	t.Setenv("LOGDIR", r.dir)
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@e")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@e")
	hook := filepath.Join(d, "hook.sh")
	saddle := filepath.Join(d, "saddle")
	write(t, hook, hookScript, 0o755)
	write(t, saddle, "#!/bin/sh\necho \"$@\" >> \"$LOGDIR/saddle-calls\"\n", 0o755)
	settings := map[string]any{"hooks": map[string]any{}}
	for _, ev := range []string{"PreToolUse", "PostToolUse", "UserPromptSubmit", "Stop", "Notification", "SessionStart"} {
		settings["hooks"].(map[string]any)[ev] = []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": hook}}}}
	}
	b, _ := json.Marshal(settings)
	write(t, filepath.Join(d, "settings.json"), string(b), 0o644)
	if err := s.Save(r.dir, "t1"); err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	r.in = pw
	r.agent = &Agent{Dir: r.dir, Task: "t1", Work: r.work, Saddle: saddle, Settings: filepath.Join(d, "settings.json"),
		Prompt: "do the thing", In: pr, Out: r.out}
	if err := r.agent.Start(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(r.dir, "t1")
	if err != nil {
		t.Fatal(err)
	}
	go func() { r.exited <- r.agent.Run(loaded) }()
	t.Cleanup(func() { _ = pw.Close(); <-r.exited })
	return r
}

func write(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

func (r *rig) log() string {
	b, _ := os.ReadFile(LogPath(r.dir, "t1"))
	return string(b)
}

// waitLog waits until the agent's log contains want.
func (r *rig) waitLog(want string) {
	r.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(r.log(), want) {
		if time.Now().After(deadline) {
			r.t.Fatalf("log never contained %q:\n%s", want, r.log())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (r *rig) hookEvents() []string {
	b, _ := os.ReadFile(filepath.Join(r.dir, "hooks.jsonl"))
	var evs []string
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var p struct {
			Event string `json:"hook_event_name"`
		}
		if json.Unmarshal([]byte(l), &p) == nil {
			evs = append(evs, p.Event)
		}
	}
	return evs
}

func TestWritesCommitsAndCallsDone(t *testing.T) {
	r := newRig(t, Script{Steps: []Step{
		Write("a.txt", "hello\n"), Commit("add a"), Done("added a"),
	}})
	r.waitLog("idle: script finished")
	if got := sh(t, r.work, "git show HEAD:a.txt"); got != "hello" {
		t.Fatalf("a.txt = %q", got)
	}
	calls, _ := os.ReadFile(filepath.Join(r.dir, "saddle-calls"))
	if strings.TrimSpace(string(calls)) != "done -s added a" {
		t.Fatalf("saddle calls = %q", calls)
	}
	evs := strings.Join(r.hookEvents(), ",")
	if !strings.HasPrefix(evs, "SessionStart,PreToolUse,PostToolUse,PreToolUse,PostToolUse,PreToolUse,PostToolUse") {
		t.Fatalf("hook events = %s", evs)
	}
	if !strings.Contains(r.log(), "prompt: do the thing") {
		t.Fatalf("prompt not logged:\n%s", r.log())
	}
}

func TestDeniedWriteStopsScript(t *testing.T) {
	r := newRig(t, Script{Steps: []Step{Write("blocked/x", "no"), Write("after", "x")}})
	r.waitLog("stuck:")
	if !strings.Contains(r.log(), "denied: [saddle] t9 owns blocked/") {
		t.Fatalf("denial not logged:\n%s", r.log())
	}
	if _, err := os.Stat(filepath.Join(r.work, "blocked", "x")); err == nil {
		t.Fatal("denied write happened")
	}
	if _, err := os.Stat(filepath.Join(r.work, "after")); err == nil {
		t.Fatal("script went on after a failed step")
	}
}

// TestBashRunsThroughPreToolUse: as in Claude Code, a Bash step runs the
// command the PreToolUse hook hands back in updatedInput, and a denied one
// doesn't run at all.
func TestBashRunsThroughPreToolUse(t *testing.T) {
	r := newRig(t, Script{Steps: []Step{Run("heavy"), Run("touch blocked/x"), Write("after", "x")}})
	r.waitLog("stuck:")
	if got := sh(t, r.work, "cat ran.txt"); got != "queued heavy" {
		t.Fatalf("ran.txt = %q, want the rewritten command's output", got)
	}
	if !strings.Contains(r.log(), "denied: [saddle] t9 owns blocked/") {
		t.Fatalf("denial not logged:\n%s", r.log())
	}
	if _, err := os.Stat(filepath.Join(r.work, "after")); err == nil {
		t.Fatal("script went on after a denied command")
	}
}

func TestOptionalStepFailureContinues(t *testing.T) {
	r := newRig(t, Script{Steps: []Step{{Run: "exit 3", Optional: true}, Write("after", "x")}})
	r.waitLog("idle: script finished")
	if _, err := os.Stat(filepath.Join(r.work, "after")); err != nil {
		t.Fatal("optional failure stopped the script")
	}
}

func TestWaitSeesStopBlockThenTypedInputAndHookContext(t *testing.T) {
	r := newRig(t, Script{Steps: []Step{
		Wait("land failed"), // delivered by the Stop hook's block
		Wait("rebase please"),
		Write("b", "b"),
	}})
	r.waitLog("step: 1 wait land failed ok")
	// The second wait went idle: Stop (no block now), prompt shown.
	deadline := time.Now().Add(10 * time.Second)
	for !strings.HasSuffix(r.out.String(), "\n> ") {
		if time.Now().After(deadline) {
			t.Fatalf("no idle prompt:\n%s", r.out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if _, err := io.WriteString(r.in, "[saddle] You have new notices.\n"); err != nil {
		t.Fatal(err)
	}
	r.waitLog("idle: script finished")
	log := r.log()
	for _, want := range []string{"stop-block: [saddle] pending: land failed", "input: [saddle] You have new notices.", "context: [saddle] notice: rebase please"} {
		if !strings.Contains(log, want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
}

func TestResolveFinishesRebase(t *testing.T) {
	r := newRig(t, Script{})
	r.waitLog("idle:")
	sh(t, r.work, `git checkout -q -b side && echo side > f && git add f && git commit -qm side &&
		git checkout -q - && echo main > f && git add f && git commit -qm main &&
		git checkout -q side && (git rebase -q master 2>/dev/null || git rebase -q main 2>/dev/null || true)`)
	if err := r.agent.step(Resolve("f", "both\n")); err != nil {
		t.Fatal(err)
	}
	if got := sh(t, r.work, "git show HEAD:f"); got != "both" {
		t.Fatalf("f = %q", got)
	}
	if st := sh(t, r.work, "git status --porcelain"); st != "" {
		t.Fatalf("not clean: %s", st)
	}
}

func TestExitStep(t *testing.T) {
	r := newRig(t, Script{Steps: []Step{Exit(7)}})
	select {
	case code := <-r.exited:
		r.exited <- code // for cleanup
		if code != 7 {
			t.Fatalf("exit = %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("agent didn't exit")
	}
}

func TestHeadlessAcknowledgesStreamJSON(t *testing.T) {
	d := t.TempDir()
	in := strings.NewReader(`{"type":"user","message":{"role":"user","content":"hello orchestrator"}}` + "\n")
	var out bytes.Buffer
	if code := Main([]string{"--script-dir", d, "-p", "--input-format", "stream-json", "--verbose"}, in, &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 || !strings.Contains(lines[0], `"subtype":"init"`) ||
		!strings.Contains(lines[1], "fake orchestrator ack: hello orchestrator") || !strings.Contains(lines[2], `"type":"result"`) {
		t.Fatalf("stream = %s", out.String())
	}
	b, _ := os.ReadFile(OrchestratorLog(d))
	if strings.TrimSpace(string(b)) != "user: hello orchestrator" {
		t.Fatalf("orchestrator log = %q", b)
	}
}

func TestVersionAnswersWithoutStartingASession(t *testing.T) {
	d := t.TempDir()
	var out bytes.Buffer
	if code := Main([]string{"--script-dir", d, "--version"}, strings.NewReader(""), &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out.String()) != Version {
		t.Fatalf("version = %q", out.String())
	}
	if ents, _ := os.ReadDir(d); len(ents) != 0 {
		t.Fatalf("--version wrote %v", ents)
	}
}

// Like Claude Code, the headless fake lists skills on disk, answers the
// initialize control request with them and runs a typed /skill.
func TestHeadlessListsAndRunsSkills(t *testing.T) {
	d, home := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	skill := filepath.Join(home, ".claude", "skills", "greet")
	if err := os.MkdirAll(skill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: greet\ndescription: Say hello\n---\nGreet $ARGUMENTS\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	in := strings.NewReader(`{"type":"control_request","request_id":"r1","request":{"subtype":"initialize"}}` + "\n" +
		`{"type":"user","message":{"role":"user","content":"/greet Ada"}}` + "\n")
	var out bytes.Buffer
	if code := Main([]string{"--script-dir", d, "-p", "--input-format", "stream-json"}, in, &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	s := out.String()
	for _, want := range []string{`"slash_commands":["greet"]`, `"type":"control_response"`, `"description":"Say hello"`, "fake skill greet ran with: Ada"} {
		if !strings.Contains(s, want) {
			t.Errorf("stream is missing %s:\n%s", want, s)
		}
	}
}

// A "slow:" message starts a turn that only an interrupt ends, with an
// error_during_execution result as Claude Code sends.
func TestHeadlessSlowTurnEndsOnInterrupt(t *testing.T) {
	d := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	in := strings.NewReader(`{"type":"user","message":{"role":"user","content":"slow: build it"}}` + "\n" +
		`{"type":"control_request","request_id":"i1","request":{"subtype":"interrupt"}}` + "\n")
	var out bytes.Buffer
	if code := Main([]string{"--script-dir", d, "-p", "--input-format", "stream-json"}, in, &out); code != 0 {
		t.Fatalf("exit %d", code)
	}
	s := out.String()
	tool, res := strings.Index(s, `"tool_use"`), strings.Index(s, `"error_during_execution"`)
	if tool < 0 || res < tool || strings.Contains(s, "fake orchestrator ack") {
		t.Fatalf("stream:\n%s", s)
	}
}
