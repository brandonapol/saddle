package usage

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func tailFixture(t *testing.T, name string, p Parser) ([]Record, Cursor) {
	t.Helper()
	var c Cursor
	recs, err := Tail(filepath.Join("testdata", name), &c, p)
	if err != nil {
		t.Fatal(err)
	}
	return recs, c
}

func TestParseCodexRollout(t *testing.T) {
	recs, c := tailFixture(t, "codex-rollout.jsonl", ParseCodex)
	if c.Malformed != 0 {
		t.Errorf("malformed = %d", c.Malformed)
	}
	// The null-info count carries nothing and the repeated count is one turn.
	if len(recs) != 2 {
		t.Fatalf("records = %+v", recs)
	}
	want := []Record{
		{Time: time.Date(2026, 10, 2, 10, 0, 9, 0, time.UTC), Model: "codex", Tokens: Tokens{Input: 200, CacheRead: 1000, Output: 80}},
		{Time: time.Date(2026, 10, 2, 10, 1, 30, 0, time.UTC), Model: "codex", Tokens: Tokens{Input: 600, CacheRead: 1900, Output: 50}},
	}
	for i, r := range recs {
		r.ID = ""
		if !r.Time.Equal(want[i].Time) || r.Model != want[i].Model || r.Tokens != want[i].Tokens {
			t.Errorf("record %d = %+v, want %+v", i, r, want[i])
		}
	}
}

func TestCodexRolloutModel(t *testing.T) {
	got, err := CodexRolloutModel(filepath.Join("testdata", "codex-rollout.jsonl"))
	if err != nil || got != "gpt-5-codex" {
		t.Fatalf("model = %q, %v", got, err)
	}
}

func TestParseGrokOutput(t *testing.T) {
	recs, c := tailFixture(t, "grok-output.jsonl", ParseGrok)
	// Plain text is console output, not a malformed transcript line.
	if c.Malformed != 0 {
		t.Errorf("malformed = %d", c.Malformed)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %+v", recs)
	}
	if r := recs[0]; r.Model != "grok-4" || r.Tokens != (Tokens{Input: 500, CacheRead: 400, Output: 60}) || !r.Time.Equal(time.Unix(1790935200, 0)) {
		t.Errorf("first = %+v", r)
	}
	if r := recs[1]; r.Model != "grok-2-image" || r.Tokens.Input != 50 {
		t.Errorf("second = %+v", r)
	}
}

func TestParseGrokContextUpdates(t *testing.T) {
	recs, c := tailFixture(t, "grok-updates.jsonl", ParseGrokContext)
	if c.Malformed != 0 || len(recs) != 3 {
		t.Fatalf("records = %+v, malformed = %d", recs, c.Malformed)
	}
	last := recs[len(recs)-1]
	if last.Model != Grok || last.Tokens.Input != 196_000 || last.ID != "event-3" {
		t.Fatalf("latest context = %+v, want the last model call's 196K prompt", last)
	}
}

func TestParseGrokUsageSnapshot(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "grok-usage.json"))
	if err != nil {
		t.Fatal(err)
	}
	recs, err := ParseGrokSnapshot(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %+v", recs)
	}
	if r := recs[0]; r.ID != "1:grok-4.7-build:2026-10-02T10:00:00Z" || r.Model != "grok-4.7-build" ||
		r.Tokens != (Tokens{Input: 500, CacheRead: 400, Output: 60}) || r.CostUSD != 0.0125 ||
		!r.Time.Equal(time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("first = %+v", r)
	}
	if r := recs[1]; r.ID != "2:grok-4.7-build:2026-10-02T10:01:00Z" || r.Tokens.Input != 50 || r.CostUSD != 0.0025 {
		t.Errorf("second = %+v", r)
	}
}

func TestCollectorTracksCodex(t *testing.T) {
	c := NewCollector()
	if !c.Track(Session{ID: "codex:t5", Task: "t5", Agent: Codex, Path: filepath.Join("testdata", "codex-rollout.jsonl")}) {
		t.Fatal("codex not tracked")
	}
	bs, err := c.Poll()
	if err != nil {
		t.Fatal(err)
	}
	var total Tokens
	for _, b := range bs {
		total = total.Add(b.Tokens)
		// #478: codex usage is labeled by provider and its rollout's model.
		if b.Adapter != Codex || b.Model != "gpt-5-codex" {
			t.Errorf("bucket = %+v, want adapter codex model gpt-5-codex", b)
		}
	}
	if total != (Tokens{Input: 800, CacheRead: 2900, Output: 130}) {
		t.Errorf("total = %+v", total)
	}
}

// #482: a rollout that switches models mid-session is split by the model of
// each turn_context, not labeled with the first one.
func TestCollectorSplitsCodexByTurnModel(t *testing.T) {
	c := NewCollector()
	if !c.Track(Session{ID: "codex:t6", Task: "t6", Agent: Codex, Path: filepath.Join("testdata", "codex-models.jsonl"), Model: "gpt-5-codex"}) {
		t.Fatal("codex not tracked")
	}
	bs, err := c.Poll()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, b := range bs {
		got[b.Model] += b.Total()
	}
	want := map[string]int64{"gpt-6-luna": 1100, "gpt-6-terra": 2200, "gpt-6-sol": 3300, "gpt-6-astra": 4400}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tokens by model = %v, want %v", got, want)
	}
}

// grokLiveUpdates is a scrubbed tail of a live grok worker's updates.jsonl
// (#598): model-call chunks carry _meta.totalTokens, the turn summary carries
// the cumulative usage the control view used to show as "?", and hook events
// carry no reading at all.
const grokLiveUpdates = `{"timestamp":1792026040,"method":"session/update","params":{"sessionId":"01a1fixture","update":{"sessionUpdate":"tool_call_update","toolCallId":"call-1","status":"completed"},"_meta":{"totalTokens":143870,"requestTrimmedTokens":0,"eventId":"01a1fixture-7998","agentTimestampMs":1792026040100,"promptId":"p1","streamStartMs":1792026039000,"turnStartMs":1792022505364,"updateType":"ToolCallUpdate","chunkId":325}}}
{"timestamp":1792026049,"method":"session/update","params":{"sessionId":"01a1fixture","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"Done."}},"_meta":{"totalTokens":144127,"requestTrimmedTokens":0,"eventId":"01a1fixture-8001","agentTimestampMs":1792026049819,"promptId":"p1","streamStartMs":1792026046358,"turnStartMs":1792022505364,"updateType":"AgentMessageChunk","chunkId":327}}}
{"timestamp":1792026049,"method":"session/update","params":{"sessionId":"01a1fixture","update":{"sessionUpdate":"hook_execution","hook":"Stop"},"_meta":{"eventId":"01a1fixture-8005","agentTimestampMs":1792026049932}}}
{"timestamp":1792026049,"method":"_x.ai/session/update","params":{"sessionId":"01a1fixture","update":{"sessionUpdate":"turn_completed","prompt_id":"p1","stop_reason":"end_turn","usage":{"inputTokens":9132422,"outputTokens":124474,"totalTokens":9256896,"cachedReadTokens":8753664,"cacheCreationTokens":0,"modelCalls":88,"modelUsage":{"grok-4.7-build":{"inputTokens":9132422,"outputTokens":124474,"totalTokens":9256896,"cachedReadTokens":8753664}}}},"_meta":{"eventId":"01a1fixture-8006","agentTimestampMs":1792026049985}}}
`

func tailString(t *testing.T, data string, p Parser) []Record {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	var c Cursor
	recs, err := Tail(path, &c, p)
	if err != nil || c.Malformed != 0 {
		t.Fatalf("tail: %v, malformed %d", err, c.Malformed)
	}
	return recs
}

// A live grok session's latest reading is the last model call's prompt, not
// the turn summary's cumulative total.
func TestParseGrokContextLiveSession(t *testing.T) {
	recs := tailString(t, grokLiveUpdates, ParseGrokContext)
	if len(recs) != 2 {
		t.Fatalf("records = %+v, want the two model-call readings", recs)
	}
	if last := recs[len(recs)-1]; last.Tokens.Input != 144_127 || last.Model != Grok {
		t.Fatalf("latest = %+v, want 144127", last)
	}
}

// After an auto-compaction the context is tokens_after until the next model
// call reports its prompt.
func TestParseGrokContextAfterCompaction(t *testing.T) {
	line := `{"timestamp":1792024252,"method":"_x.ai/session/update","params":{"sessionId":"01a1fixture","update":{"sessionUpdate":"auto_compact_completed","tokens_before":206933,"tokens_after":22531,"elapsed_ms":78510,"summary_preview":null},"_meta":{"eventId":"01a1fixture-6971","agentTimestampMs":1792024252141}}}`
	rec, ok, err := ParseGrokContext([]byte(line))
	if err != nil || !ok || rec.Tokens.Input != 22_531 {
		t.Fatalf("compaction = %+v, %v, %v; want 22531", rec, ok, err)
	}
	started := `{"timestamp":1792024173,"method":"_x.ai/session/update","params":{"sessionId":"01a1fixture","update":{"sessionUpdate":"auto_compact_started","tokens_used":206933,"context_window":256000,"percentage":81},"_meta":{"eventId":"01a1fixture-6969"}}}`
	if _, ok, err := ParseGrokContext([]byte(started)); ok || err != nil {
		t.Fatalf("auto_compact_started gave a reading (%v, %v)", ok, err)
	}
}

// codexLiveRollout is a scrubbed slice of a live codex worker's rollout: the
// turn's model, then token counts whose last_token_usage is one model call
// and model_context_window the window Codex sizes against.
const codexLiveRollout = `{"timestamp":"2026-10-14T14:22:37.100Z","type":"session_meta","payload":{"id":"01a1fixture","cwd":"/work/t1","originator":"codex_cli_rs","cli_version":"0.130.0"}}
{"timestamp":"2026-10-14T14:22:39.478Z","ordinal":7,"type":"turn_context","payload":{"turn_id":"turn-1","cwd":"/work/t1","approval_policy":"never","model":"gpt-5.6-sol","effort":"high"}}
{"timestamp":"2026-10-14T14:39:50.001Z","ordinal":366,"type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"limit_id":"codex","primary":{"used_percent":25.0,"window_minutes":300}}}}
{"timestamp":"2026-10-14T14:39:55.469Z","ordinal":368,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":4298911,"cached_input_tokens":4215296,"cache_write_input_tokens":0,"output_tokens":9011,"reasoning_output_tokens":3702,"total_tokens":4307922},"last_token_usage":{"input_tokens":83986,"cached_input_tokens":83584,"cache_write_input_tokens":0,"output_tokens":133,"reasoning_output_tokens":41,"total_tokens":84119},"model_context_window":258400},"rate_limits":{"limit_id":"codex","primary":{"used_percent":25.0,"window_minutes":300}}}}
`

func TestParseCodexContextLiveRollout(t *testing.T) {
	var got []ContextReading
	for _, line := range strings.Split(strings.TrimSpace(codexLiveRollout), "\n") {
		r, ok, err := ParseCodexContext([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			got = append(got, r)
		}
	}
	want := []ContextReading{
		{Model: "gpt-5.6-sol"},
		{Time: time.Date(2026, 10, 14, 14, 39, 55, 469_000_000, time.UTC), Prompt: 83_986, Window: 258_400},
	}
	if len(got) != len(want) {
		t.Fatalf("readings = %+v, want %+v", got, want)
	}
	for i := range want {
		if !got[i].Time.Equal(want[i].Time) || got[i].Model != want[i].Model || got[i].Prompt != want[i].Prompt || got[i].Window != want[i].Window {
			t.Errorf("reading %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}
