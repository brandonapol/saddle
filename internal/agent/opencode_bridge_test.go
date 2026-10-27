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

// fakeOpenCodeRun is an opencode that answers `run --format json [-m M]
// [-s id]` with the message on stdin, in the event shapes opencode 1.19.0
// prints. A new session gets the id ses_N, N counting calls; -s keeps the
// id. Each call's argv and prompt are appended to calls.log. -s bad fails
// as opencode does for an unknown session (no JSON, "Session not found" on
// stderr); a prompt with QUOTA fails the turn with Zen's balance error, and
// one with ZENFREE with Zen's free-tier rate limit, whose quota type is only
// in the response body.
const fakeOpenCodeRun = `#!/bin/sh
dir=$(dirname "$0")
prompt=$(cat)
sid=
prev=
for a in "$@"; do
  if [ "$prev" = "-s" ]; then sid=$a; fi
  prev=$a
done
n=$(wc -l < "$dir/calls.log" 2>/dev/null || echo 0)
printf '%s\n' "$* | $(printf '%s' "$prompt" | tr '\n' ' ')" >> "$dir/calls.log"
if [ "$sid" = "bad" ]; then
  printf '\033[91m\033[1mError: \033[0mSession not found\n' >&2
  exit 1
fi
sid=${sid:-ses_$n}
last=$(printf '%s' "$prompt" | tail -n 1)
ev() { printf '{"type":"%s","timestamp":1793065129707,"sessionID":"%s","part":%s}\n' "$1" "$sid" "$2"; }
ev step_start '{"type":"step-start"}'
case "$prompt" in *QUOTA*)
  printf '{"type":"error","timestamp":1,"sessionID":"%s","error":{"name":"APIError","data":{"message":"Insufficient balance. Manage your billing here: https://opencode.ai/workspace/billing","statusCode":401}}}\n' "$sid"
  exit 1
esac
case "$prompt" in *ZENFREE*)
  printf '{"type":"error","timestamp":1,"sessionID":"%s","error":{"name":"APIError","data":{"message":"Rate limit exceeded. Please try again later.","statusCode":429,"responseBody":"{\\"type\\":\\"error\\",\\"error\\":{\\"type\\":\\"FreeUsageLimitError\\",\\"message\\":\\"Rate limit exceeded. Please try again later.\\"}}"}}}\n' "$sid"
  exit 1
esac
ev tool_use '{"type":"tool","tool":"saddle_status","callID":"call_1","state":{"status":"completed","input":{"ids":["t1"]},"output":"ok"},"id":"prt_1"}'
ev tool_use '{"type":"tool","tool":"bash","callID":"call_2","state":{"status":"completed","input":{"command":"echo hi"},"output":"hi"},"id":"prt_2"}'
ev step_finish '{"id":"prt_3","type":"step-finish","reason":"tool-calls","tokens":{"total":110,"input":100,"output":5,"reasoning":5,"cache":{"write":0,"read":0}}}'
ev text "{\"id\":\"prt_4\",\"type\":\"text\",\"text\":\"said=$last on=$sid\"}"
ev step_finish "{\"id\":\"prt_5\",\"type\":\"step-finish\",\"reason\":\"stop\",\"tokens\":{\"total\":9000,\"input\":20,\"output\":8,\"reasoning\":2,\"cache\":{\"write\":30,\"read\":${TOKENS:-8000}}}}"
`

func openCodeBridgeDir(t *testing.T, spec openCodeSpec) string {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "opencode")
	if err := os.WriteFile(fake, []byte(fakeOpenCodeRun), 0o755); err != nil {
		t.Fatal(err)
	}
	spec.Cmd, spec.Dir = fake, dir
	if spec.Context == "" {
		spec.Context = filepath.Join(dir, OpenCodeContextFile)
	}
	b, _ := json.Marshal(spec)
	if err := os.WriteFile(filepath.Join(dir, openCodeSpecFile), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestOpenCodeBridgeResumesAcrossTurns(t *testing.T) {
	dir := openCodeBridgeDir(t, openCodeSpec{Brief: "BRIEF", Model: "opencode/big-pickle", Args: []string{"--auto"}})
	var out bytes.Buffer
	if err := RunOpenCodeBridge(dir, userLines("hello", "again"), &out); err != nil {
		t.Fatal(err)
	}
	calls := readCalls(t, dir)
	if len(calls) != 2 {
		t.Fatalf("want two turns:\n%s", strings.Join(calls, "\n"))
	}
	if !strings.HasPrefix(calls[0], "run --format json -m opencode/big-pickle --auto |") || !strings.Contains(calls[0], "BRIEF") {
		t.Errorf("first turn = %q, want a new session with the model, the approval flag and the brief", calls[0])
	}
	if !strings.Contains(calls[1], "-s ses_0") || strings.Contains(calls[1], "BRIEF") {
		t.Errorf("second turn = %q, want ses_0 resumed without the brief", calls[1])
	}
	ls := wireLines(t, out.String())
	var inits, results, texts []string
	var tools []string
	for _, l := range ls {
		switch l.Type {
		case "system":
			inits = append(inits, l.SessionID)
		case "result":
			if l.IsError {
				t.Errorf("unexpected error result: %+v", l)
			}
			results = append(results, l.Result)
		case "assistant":
			for _, c := range l.Message.Content {
				if c.Type == "tool_use" {
					tools = append(tools, c.Name+" "+string(c.Input))
				} else {
					texts = append(texts, c.Text)
				}
			}
		}
	}
	if !slices.Equal(inits, []string{"ses_0", "ses_0"}) {
		t.Errorf("init sessions = %q", inits)
	}
	if !slices.Equal(results, []string{"said=hello on=ses_0", "said=again on=ses_0"}) {
		t.Errorf("results = %q", results)
	}
	if len(tools) != 4 || tools[0] != `mcp__saddle__status {"ids":["t1"]}` || tools[1] != `Bash {"command":"echo hi"}` {
		t.Errorf("tool_use blocks = %q, want saddle_status as mcp__saddle__status and bash as Bash", tools)
	}
	if len(texts) != 2 {
		t.Errorf("texts = %q", texts)
	}
}

// A result carries the last model call's usage in Claude's shape.
func TestOpenCodeBridgeResultUsage(t *testing.T) {
	dir := openCodeBridgeDir(t, openCodeSpec{})
	var out bytes.Buffer
	if err := RunOpenCodeBridge(dir, userLines("hello"), &out); err != nil {
		t.Fatal(err)
	}
	var res struct {
		Usage map[string]int64 `json:"usage"`
	}
	for _, s := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.Contains(s, `"type":"result"`) {
			_ = json.Unmarshal([]byte(s), &res)
		}
	}
	want := map[string]int64{"input_tokens": 20, "output_tokens": 10, "cache_read_input_tokens": 8000, "cache_creation_input_tokens": 30}
	for k, v := range want {
		if res.Usage[k] != v {
			t.Errorf("usage[%s] = %d, want %d (%v)", k, res.Usage[k], v, res.Usage)
		}
	}
}

func TestOpenCodeBridgeStartsFreshWhenSessionMissing(t *testing.T) {
	dir := openCodeBridgeDir(t, openCodeSpec{Resume: "bad", Brief: "BRIEF"})
	var out bytes.Buffer
	if err := RunOpenCodeBridge(dir, userLines("hello", "again"), &out); err != nil {
		t.Fatal(err)
	}
	calls := readCalls(t, dir)
	if len(calls) != 3 || !strings.Contains(calls[0], "-s bad") || strings.Contains(calls[1], "-s ") ||
		!strings.Contains(calls[1], "BRIEF") || !strings.Contains(calls[2], "-s ses_1") {
		t.Fatalf("want a failed resume, a fresh session with the brief, then resuming it:\n%s", strings.Join(calls, "\n"))
	}
	if strings.Contains(out.String(), `"is_error":true`) {
		t.Errorf("a recovered resume should not show an error:\n%s", out.String())
	}
}

// A quota error reaches the orchestrator as an error result its failover
// recognises, including wordings usage.DetectLimitBanner doesn't know on
// their own: Zen's balance error and its free tier's bare rate limit.
func TestOpenCodeBridgeReportsQuotaError(t *testing.T) {
	for _, prompt := range []string{"QUOTA", "ZENFREE"} {
		t.Run(prompt, func(t *testing.T) {
			dir := openCodeBridgeDir(t, openCodeSpec{})
			var out bytes.Buffer
			if err := RunOpenCodeBridge(dir, userLines(prompt), &out); err != nil {
				t.Fatal(err)
			}
			ls := wireLines(t, out.String())
			last := ls[len(ls)-1]
			if last.Type != "result" || !last.IsError || last.SessionID != "ses_0" {
				t.Fatalf("want an error result on ses_0, got %+v\n%s", last, out.String())
			}
			if _, ok := usage.DetectLimitBanner(last.Result); !ok {
				t.Errorf("failover can't read %q as out of quota", last.Result)
			}
			if calls := readCalls(t, dir); len(calls) != 1 {
				t.Errorf("a quota error with a session must not retry fresh:\n%s", strings.Join(calls, "\n"))
			}
		})
	}
}

func TestOpenCodeQuotaText(t *testing.T) {
	for _, tc := range []struct{ msg, body, want string }{
		{"Unexpected server error. Check server logs for details.", "", "Unexpected server error. Check server logs for details."},
		{"Free usage exceeded, subscribe to Go", "", "Free usage exceeded, subscribe to Go"},
		{"5 hour usage limit reached. It will reset in 2 hours 5 minutes.", "", "5 hour usage limit reached. It will reset in 2 hours 5 minutes."},
		{"Insufficient balance.", "", "opencode quota exceeded: Insufficient balance."},
		{"Go usage limit reached. Resets in 2hr 5min", "", "Go usage limit reached. Resets in 2hr 5min"},
	} {
		if got := openCodeQuotaText(tc.msg, tc.body); got != tc.want {
			t.Errorf("openCodeQuotaText(%q) = %q, want %q", tc.msg, got, tc.want)
		}
	}
}

func TestOpenCodeBridgeCompactStartsFreshSessionFromSummary(t *testing.T) {
	dir := openCodeBridgeDir(t, openCodeSpec{Brief: "BRIEF"})
	var out bytes.Buffer
	if err := RunOpenCodeBridge(dir, userLines("hello", "/compact keep the PRs", "after"), &out); err != nil {
		t.Fatal(err)
	}
	calls := readCalls(t, dir)
	if len(calls) != 4 {
		t.Fatalf("want a turn, a summary turn, a fresh session and a turn on it:\n%s", strings.Join(calls, "\n"))
	}
	if !strings.Contains(calls[1], "-s ses_0") || !strings.Contains(calls[1], "being compacted") || !strings.Contains(calls[1], "Keep: keep the PRs") {
		t.Errorf("summary turn = %q", calls[1])
	}
	if strings.Contains(calls[2], "-s ") || !strings.Contains(calls[2], "BRIEF") || !strings.Contains(calls[2], "continues one that was compacted") {
		t.Errorf("fresh session = %q, want the brief and the summary", calls[2])
	}
	if !strings.Contains(calls[3], "-s ses_2") {
		t.Errorf("next turn = %q, want the fresh session resumed", calls[3])
	}
	if !strings.Contains(out.String(), `"compact_boundary"`) {
		t.Errorf("no compact_boundary:\n%s", out.String())
	}
}

// Each model call's tokens land in the context ledger, which the context
// meter reads as Claude transcript lines: the last is the session's prompt.
func TestOpenCodeBridgeWritesContextLedger(t *testing.T) {
	dir := openCodeBridgeDir(t, openCodeSpec{Model: "anthropic/claude-sonnet-4-5"})
	t.Setenv("TOKENS", "150000")
	var out bytes.Buffer
	if err := RunOpenCodeBridge(dir, userLines("hello"), &out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, OpenCodeContextFile))
	if err != nil {
		t.Fatal(err)
	}
	var recs []usage.Record
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		r, ok, err := usage.ParseClaude([]byte(l))
		if err != nil || !ok {
			t.Fatalf("ledger line %q: ok=%v err=%v", l, ok, err)
		}
		recs = append(recs, r)
	}
	if len(recs) != 2 {
		t.Fatalf("want one record per step, got %d", len(recs))
	}
	last := recs[1]
	if last.Model != "anthropic/claude-sonnet-4-5" || last.Tokens.Input+last.Tokens.CacheRead+last.Tokens.CacheCreation != 150050 {
		t.Errorf("last record = %+v, want the model and a 150050-token prompt", last)
	}
}

func TestHeadlessOpenCodeWritesSpec(t *testing.T) {
	run := t.TempDir()
	l := Launch{
		Root: "/repo", Bin: "/bin/saddle", Task: "orchestrator", Dir: "/repo", RunDir: run, Kind: KindOpenCode,
		Model: "opencode/big-pickle", Brief: "BRIEF", Mode: "auto", Deny: OrchestratorDeny(),
		Args: []string{"--agent", "build", "--prompt", "x", "--pure"},
	}
	cmd, err := l.Headless("ses_9")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cmd.Args, []string{"/bin/saddle", "opencode-bridge", run}) {
		t.Errorf("cmd = %q", cmd.Args)
	}
	if got := Recorded(run); got != usage.OpenCode {
		t.Errorf("recorded agent = %q", got)
	}
	var spec openCodeSpec
	b, err := os.ReadFile(filepath.Join(run, openCodeSpecFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.Cmd != "opencode" || spec.Resume != "ses_9" || spec.Brief != "BRIEF" || spec.Model != "opencode/big-pickle" {
		t.Errorf("spec = %+v", spec)
	}
	if want := []string{"--auto", "--agent", "build", "--pure"}; !slices.Equal(spec.Args, want) {
		t.Errorf("args = %q, want %q (TUI-only --prompt dropped)", spec.Args, want)
	}
	conf := filepath.Join(run, openCodeConfig)
	if !slices.Contains(spec.Env, "OPENCODE_CONFIG="+conf) || !slices.Contains(spec.Env, "SADDLE_TASK=orchestrator") {
		t.Errorf("env = %q", spec.Env)
	}
	var cfg struct {
		MCP map[string]struct {
			Command     []string          `json:"command"`
			Environment map[string]string `json:"environment"`
		} `json:"mcp"`
		Permission struct {
			Edit string            `json:"edit"`
			Task string            `json:"task"`
			Bash map[string]string `json:"bash"`
		} `json:"permission"`
	}
	b, err = os.ReadFile(conf)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	if s := cfg.MCP["saddle"]; !slices.Equal(s.Command, []string{"/bin/saddle", "mcp"}) || s.Environment["SADDLE_TASK"] != "orchestrator" {
		t.Errorf("mcp = %+v", cfg.MCP)
	}
	p := cfg.Permission
	if p.Edit != "deny" || p.Task != "deny" || p.Bash["git push*"] != "deny" || p.Bash["git checkout*"] != "deny" {
		t.Errorf("permission = %+v, want the orchestrator deny list", p)
	}
}

func TestOpenCodeToolName(t *testing.T) {
	for in, want := range map[string]string{"bash": "Bash", "saddle_spawn": "mcp__saddle__spawn", "write": "Write", "github_search": "github_search"} {
		if got := openCodeToolName(in); got != want {
			t.Errorf("openCodeToolName(%q) = %q, want %q", in, got, want)
		}
	}
}
