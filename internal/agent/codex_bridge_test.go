package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/usage"
)

// fakeCodex is a codex that answers `exec --json [resume <id>] -`. A new
// session gets the id thread-N, N counting calls; a resumed one keeps its
// id. Each call's argv and prompt are appended to calls.log. Resuming "bad"
// fails like a missing rollout; a prompt with QUOTA fails the turn the way
// codex reports a usage limit.
const fakeCodex = `#!/bin/sh
dir=$(dirname "$0")
prompt=$(cat)
resume=
prev=
for a in "$@"; do
  if [ "$prev" = "resume" ]; then resume=$a; fi
  prev=$a
done
n=$(wc -l < "$dir/calls.log" 2>/dev/null || echo 0)
printf '%s\n' "$* | $(printf '%s' "$prompt" | tr '\n' ' ')" >> "$dir/calls.log"
if [ "$resume" = "bad" ]; then
  echo "Error: thread/resume failed: no rollout found for thread id bad" >&2
  exit 1
fi
sid=${resume:-thread-$n}
first=$(printf '%s' "$prompt" | tail -n 1)
printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$sid\"}"
printf '%s\n' '{"type":"turn.started"}'
case "$prompt" in *QUOTA*)
  printf '%s\n' '{"type":"error","message":"Reconnecting... 1/5"}'
  printf '%s\n' "{\"type\":\"turn.failed\",\"error\":{\"message\":\"You've hit your usage limit. Try again at 3:04 PM.\"}}"
  exit 1
esac
printf '%s\n' '{"type":"item.started","item":{"id":"item_1","type":"mcp_tool_call","server":"saddle","tool":"status","arguments":{"ids":["t1"]},"status":"in_progress"}}'
printf '%s\n' '{"type":"item.completed","item":{"id":"item_1","type":"mcp_tool_call","server":"saddle","tool":"status","arguments":{"ids":["t1"]},"status":"completed"}}'
printf '%s\n' "{\"type\":\"item.completed\",\"item\":{\"id\":\"item_2\",\"type\":\"agent_message\",\"text\":\"said=$first on=$sid\"}}"
printf '%s\n' '{"type":"turn.completed","usage":{"input_tokens":10,"output_tokens":2}}'
`

func codexBridgeDir(t *testing.T, spec codexSpec) string {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	if err := os.WriteFile(fake, []byte(fakeCodex), 0o755); err != nil {
		t.Fatal(err)
	}
	spec.Cmd, spec.Dir = fake, dir
	b, _ := json.Marshal(spec)
	if err := os.WriteFile(filepath.Join(dir, "codex.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := (Launch{RunDir: dir}).record(usage.Codex); err != nil {
		t.Fatal(err)
	}
	return dir
}

func userLines(texts ...string) *strings.Reader {
	var lines []string
	for _, s := range texts {
		b, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": s}})
		lines = append(lines, string(b))
	}
	return strings.NewReader(strings.Join(lines, "\n") + "\n")
}

func readCalls(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

type wireLine struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Result    string `json:"result"`
	IsError   bool   `json:"is_error"`
	Message   struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
}

func wireLines(t *testing.T, out string) []wireLine {
	t.Helper()
	var ls []wireLine
	for _, s := range strings.Split(strings.TrimSpace(out), "\n") {
		var l wireLine
		if err := json.Unmarshal([]byte(s), &l); err != nil {
			t.Fatalf("not JSON: %q", s)
		}
		ls = append(ls, l)
	}
	return ls
}

func TestCodexBridgeResumesAcrossTurns(t *testing.T) {
	dir := codexBridgeDir(t, codexSpec{Brief: "BRIEF", Args: []string{"-c", "x=1"}, Model: "gpt-x"})
	var out bytes.Buffer
	if err := RunCodexBridge(dir, userLines("first", "second", "third"), &out); err != nil {
		t.Fatal(err)
	}
	calls := readCalls(t, dir)
	if len(calls) != 3 {
		t.Fatalf("want 3 codex turns, got:\n%s", strings.Join(calls, "\n"))
	}
	if !strings.HasPrefix(calls[0], "exec --json --skip-git-repo-check -c x=1 - | BRIEF") || !strings.Contains(calls[0], "first") {
		t.Errorf("first turn should start a session with the brief: %s", calls[0])
	}
	for _, c := range calls[1:] {
		if !strings.Contains(c, "-c x=1 resume thread-0 - | ") || strings.Contains(c, "BRIEF") {
			t.Errorf("later turns should resume thread-0 without the brief: %s", c)
		}
	}
	var texts, tools, results []string
	inits := 0
	for _, l := range wireLines(t, out.String()) {
		switch l.Type {
		case "system":
			if l.Subtype == "init" && l.SessionID == "thread-0" {
				inits++
			}
		case "assistant":
			for _, c := range l.Message.Content {
				switch c.Type {
				case "text":
					texts = append(texts, c.Text)
				case "tool_use":
					tools = append(tools, c.Name+" "+string(c.Input))
				}
			}
		case "result":
			if l.IsError || l.SessionID != "thread-0" {
				t.Errorf("bad result: %+v", l)
			}
			results = append(results, l.Result)
		}
	}
	if inits != 3 {
		t.Errorf("want an init line per turn on thread-0, got %d", inits)
	}
	want := []string{"said=first on=thread-0", "said=second on=thread-0", "said=third on=thread-0"}
	if !slices.Equal(texts, want) || !slices.Equal(results, want) {
		t.Errorf("texts %q results %q, want %q", texts, results, want)
	}
	if len(tools) != 3 || tools[0] != `mcp__saddle__status {"ids":["t1"]}` {
		t.Errorf("each MCP call should be one tool_use: %q", tools)
	}
}

func TestCodexBridgeStartsFreshWhenResumeFails(t *testing.T) {
	dir := codexBridgeDir(t, codexSpec{Resume: "bad"})
	var out bytes.Buffer
	if err := RunCodexBridge(dir, userLines("hello", "again"), &out); err != nil {
		t.Fatal(err)
	}
	calls := readCalls(t, dir)
	if len(calls) != 3 || !strings.Contains(calls[0], "resume bad") || strings.Contains(calls[1], "resume") ||
		!strings.Contains(calls[2], "resume thread-1") {
		t.Fatalf("want a failed resume, a fresh session, then resuming it:\n%s", strings.Join(calls, "\n"))
	}
	if strings.Contains(out.String(), `"is_error":true`) {
		t.Errorf("a recovered resume should not show an error:\n%s", out.String())
	}
}

func TestCodexBridgeReportsQuotaError(t *testing.T) {
	dir := codexBridgeDir(t, codexSpec{})
	var out bytes.Buffer
	if err := RunCodexBridge(dir, userLines("hi", "QUOTA please"), &out); err != nil {
		t.Fatal(err)
	}
	ls := wireLines(t, out.String())
	last := ls[len(ls)-1]
	if last.Type != "result" || !last.IsError || !strings.Contains(last.Result, "usage limit") {
		t.Fatalf("want the usage-limit message as an error result, got %+v", last)
	}
	if _, ok := usage.DetectLimitBanner(last.Result); !ok {
		t.Errorf("failover can't read %q as a quota banner", last.Result)
	}
	if calls := readCalls(t, dir); len(calls) != 2 {
		t.Errorf("a failed turn on a live session should not retry fresh:\n%s", strings.Join(calls, "\n"))
	}
}

func TestCodexBridgeCompactStartsFreshSessionFromSummary(t *testing.T) {
	dir := codexBridgeDir(t, codexSpec{Brief: "BRIEF"})
	var out bytes.Buffer
	if err := RunCodexBridge(dir, userLines("hi", "/compact keep tasks", "after"), &out); err != nil {
		t.Fatal(err)
	}
	calls := readCalls(t, dir)
	if len(calls) != 4 {
		t.Fatalf("want turn, summary, fresh session, turn:\n%s", strings.Join(calls, "\n"))
	}
	if !strings.Contains(calls[1], "resume thread-0") || !strings.Contains(calls[1], "Keep: keep tasks") {
		t.Errorf("summary should be asked of the old session: %s", calls[1])
	}
	if strings.Contains(calls[2], "resume") || !strings.Contains(calls[2], "BRIEF") ||
		!strings.Contains(calls[2], "Its summary: said=") ||
		!strings.Contains(calls[2], "Keep: keep tasks on=thread-0") {
		t.Errorf("fresh session should open with the brief and the summary: %s", calls[2])
	}
	if !strings.Contains(calls[3], "resume thread-2 - | after") {
		t.Errorf("the next message should resume the fresh session: %s", calls[3])
	}
	boundary := false
	for _, l := range wireLines(t, out.String()) {
		if l.Type == "system" && l.Subtype == "compact_boundary" && l.SessionID == "thread-2" {
			boundary = true
		}
	}
	if !boundary {
		t.Errorf("no compact_boundary for the fresh session:\n%s", out.String())
	}
	if strings.Contains(out.String(), "Reply with the summary only") {
		t.Errorf("the compaction turns should not show in the chat:\n%s", out.String())
	}
}

func TestCodexExecArgs(t *testing.T) {
	l := Launch{Bin: "/bin/saddle", Root: "/r", Task: "orchestrator", Model: "gpt-x",
		Args: []string{"--search", "-c", "foo=1", "--enable=bar", "-a", "never"}}
	got := strings.Join(l.codexExecArgs(), " ")
	for _, want := range []string{
		`-c mcp_servers.saddle.command="/bin/saddle"`, `-c mcp_servers.saddle.args=["mcp"]`,
		`SADDLE_ROOT="/r",SADDLE_TASK="orchestrator"`, "-m gpt-x",
		"--dangerously-bypass-approvals-and-sandbox", "-c foo=1", "--enable=bar",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	if strings.Contains(got, "--search") || strings.Contains(got, " -a ") {
		t.Errorf("interactive-only flags reached exec: %s", got)
	}
	l.Mode = "acceptEdits"
	if got := strings.Join(l.codexExecArgs(), " "); !strings.Contains(got, `sandbox_mode="workspace-write"`) {
		t.Errorf("acceptEdits: %s", got)
	}
	l.Mode, l.Args = "", []string{"-c", `sandbox_mode="read-only"`}
	if got := strings.Join(l.codexExecArgs(), " "); strings.Contains(got, "dangerously") {
		t.Errorf("args that set the sandbox should win: %s", got)
	}
}

func TestHeadlessCodexWritesSpec(t *testing.T) {
	run := t.TempDir()
	l := Launch{Kind: KindCodex, Bin: "/bin/saddle", Root: "/r", Task: "orchestrator", Dir: "/r", RunDir: run, Brief: "B", Cmd: "mycodex"}
	cmd, err := l.Headless("sess-1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cmd.Args, []string{"/bin/saddle", "codex-bridge", run}) {
		t.Errorf("args = %q", cmd.Args)
	}
	if Recorded(run) != usage.Codex {
		t.Errorf("run dir recorded %q", Recorded(run))
	}
	b, err := os.ReadFile(filepath.Join(run, "codex.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec codexSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Cmd != "mycodex" || spec.Resume != "sess-1" || spec.Brief != "B" || spec.Dir != "/r" {
		t.Errorf("spec = %+v", spec)
	}
}

// #668: codex hands MCP servers only the env its config names, so saddle up's
// harness fallback must ride in mcp_servers.saddle.env, or the orchestrator's
// spawns default to the claude it fell back from.
func TestCodexMCPEnvCarriesHarness(t *testing.T) {
	t.Setenv("SADDLE_HARNESS", "codex")
	l := Launch{Bin: "/bin/saddle", Root: "/r", Task: "orchestrator"}
	want := `mcp_servers.saddle.env={SADDLE_ROOT="/r",SADDLE_TASK="orchestrator",SADDLE_HARNESS="codex"}`
	if got := strings.Join(l.codexExecArgs(), " "); !strings.Contains(got, want) {
		t.Errorf("exec args lack %s: %s", want, got)
	}
	_, script, _ := launchScript(t, Codex{}, Launch{Bin: "/bin/saddle", Root: "/r", Task: "t1", Dir: t.TempDir(), RunDir: t.TempDir()})
	if !strings.Contains(script, `SADDLE_HARNESS=\"codex\"`) && !strings.Contains(script, `SADDLE_HARNESS="codex"`) {
		t.Errorf("worker launch lacks the harness in the MCP env:\n%s", script)
	}
}
