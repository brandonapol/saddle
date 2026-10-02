package usage

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func fixture(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/session.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "s1.jsonl")
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPollBucketsModelsAndMalformed(t *testing.T) {
	p := fixture(t)
	c := NewCollector()
	c.Track(Session{ID: "s1", Task: "t3", Agent: Claude, Path: p})
	got, err := c.Poll()
	if err != nil {
		t.Fatal(err)
	}
	// 10:00 opus (m1 once), 10:00 haiku, 10:01 opus (m3).
	if len(got) != 3 {
		t.Fatalf("want 3 buckets, got %+v", got)
	}
	m0 := ts("2026-01-01T10:00:00Z")
	if got[0].Minute != m0 || got[0].Model != "claude-haiku-4-5-20251001" || got[0].Input != 1 || got[0].Output != 2 {
		t.Errorf("bucket0 = %+v", got[0])
	}
	want := Tokens{Input: 10, Output: 20, CacheRead: 1000, CacheCreation: 100}
	if got[1].Minute != m0 || got[1].Model != "claude-opus-5-5" || got[1].Tokens != want || got[1].Messages != 1 {
		t.Errorf("bucket1 = %+v", got[1])
	}
	if got[2].Minute != ts("2026-01-01T10:01:00Z") || got[2].Task != "t3" || got[2].Input != 5 {
		t.Errorf("bucket2 = %+v", got[2])
	}
	cur, _ := c.Cursor("s1")
	if cur.Malformed != 1 {
		t.Errorf("malformed = %d", cur.Malformed)
	}
}

func TestIncrementalNoDoubleCount(t *testing.T) {
	p := fixture(t)
	c := NewCollector()
	c.Track(Session{ID: "s1", Task: "t3", Agent: Claude, Path: p})
	if _, err := c.Poll(); err != nil {
		t.Fatal(err)
	}
	again, _ := c.Poll()
	if len(again) != 0 {
		t.Fatalf("re-poll returned %+v", again)
	}

	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	defer f.Close()
	// Partial line: no newline yet, must not be consumed.
	line := `{"type":"assistant","timestamp":"2026-01-01T10:02:00Z","message":{"id":"m5","model":"claude-opus-5-5","usage":{"input_tokens":7,"output_tokens":1}}}`
	write(t, f, line[:60])
	if got, _ := c.Poll(); len(got) != 0 {
		t.Fatalf("partial line consumed: %+v", got)
	}
	write(t, f, line[60:]+"\n")
	got, _ := c.Poll()
	if len(got) != 1 || got[0].Input != 7 || got[0].Minute != ts("2026-01-01T10:02:00Z") {
		t.Fatalf("after completion: %+v", got)
	}
	// Duplicate of m5 appended later is ignored across polls.
	write(t, f, line+"\n")
	if got, _ := c.Poll(); len(got) != 0 {
		t.Fatalf("dup across polls counted: %+v", got)
	}
}

func TestTruncationResets(t *testing.T) {
	p := fixture(t)
	c := NewCollector()
	c.Track(Session{ID: "s1", Task: "t3", Agent: Claude, Path: p})
	_, _ = c.Poll()
	line := `{"type":"assistant","timestamp":"2026-01-01T11:00:00Z","message":{"id":"x","model":"m","usage":{"input_tokens":3}}}` + "\n"
	if err := os.WriteFile(p, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	got, _ := c.Poll()
	if len(got) != 1 || got[0].Input != 3 {
		t.Fatalf("got %+v", got)
	}
}

func TestMissingFileAndNoTimestamp(t *testing.T) {
	c := NewCollector()
	c.Now = func() time.Time { return ts("2026-02-02T03:04:59Z") }
	p := filepath.Join(t.TempDir(), "later.jsonl")
	c.Track(Session{ID: "s", Task: "t", Agent: Claude, Path: p})
	if got, err := c.Poll(); err != nil || len(got) != 0 {
		t.Fatalf("missing: %v %v", got, err)
	}
	mustErr(t, os.WriteFile(p, []byte(`{"message":{"model":"m","usage":{"output_tokens":4}}}`+"\n"), 0o644))
	got, _ := c.Poll()
	if len(got) != 1 || got[0].Minute != ts("2026-02-02T03:04:00Z") || got[0].Output != 4 {
		t.Fatalf("got %+v", got)
	}
}

func TestMultipleSessionsSameBucketMerge(t *testing.T) {
	dir := t.TempDir()
	l := `{"timestamp":"2026-01-01T10:00:01Z","message":{"id":"%s","model":"m","usage":{"input_tokens":2}}}` + "\n"
	a, b := filepath.Join(dir, "a.jsonl"), filepath.Join(dir, "b.jsonl")
	mustErr(t, os.WriteFile(a, []byte(fmt.Sprintf(l, "a1")), 0o644))
	mustErr(t, os.WriteFile(b, []byte(fmt.Sprintf(l, "b1")), 0o644))
	c := NewCollector()
	c.Track(Session{ID: "a", Task: "t1", Agent: Claude, Path: a})
	c.Track(Session{ID: "b", Task: "t1", Agent: Claude, Path: b})
	got, _ := c.Poll()
	if len(got) != 1 || got[0].Input != 4 || got[0].Messages != 2 {
		t.Fatalf("got %+v", got)
	}
}

func TestStubsAndUnknownAgent(t *testing.T) {
	c := NewCollector()
	if c.Track(Session{ID: "x", Agent: "nope"}) {
		t.Error("unknown agent tracked")
	}
	if !c.Track(Session{ID: Codex, Task: "t", Agent: Codex, Path: fixture(t)}) {
		t.Error("codex not tracked")
	}
	if got, _ := c.Poll(); len(got) != 0 {
		t.Errorf("codex stub produced %+v", got)
	}
}

func TestParseGrokUsageEvent(t *testing.T) {
	rec, ok, err := ParseGrok([]byte(`{"type":"usage","messageId":"resp_1","usage":{"input_tokens":3,"output_tokens":4,"cache_read_input_tokens":5}}`))
	if err != nil || !ok || rec.ID != "resp_1" || rec.Tokens.Input != 3 || rec.Tokens.Output != 4 || rec.Tokens.CacheRead != 5 {
		t.Fatalf("usage event: %+v ok=%v err=%v", rec, ok, err)
	}
	// streaming-messages-json assistant lines match Claude's transcript shape.
	rec, ok, err = ParseGrok([]byte(`{"type":"assistant","message":{"id":"m","model":"grok-4.5","usage":{"input_tokens":1,"output_tokens":2}}}`))
	if err != nil || !ok || rec.Model != "grok-4.5" || rec.Tokens.Input != 1 {
		t.Fatalf("assistant line: %+v ok=%v err=%v", rec, ok, err)
	}
}

func TestTranscriptPath(t *testing.T) {
	got := ClaudeTranscriptPath("/c", "/home/u/code/my.repo", "sid")
	if want := "/c/projects/-home-u-code-my-repo/sid.jsonl"; got != want {
		t.Errorf("got %s want %s", got, want)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "projects", "proj", "sid.jsonl")
	mustErr(t, os.MkdirAll(filepath.Dir(p), 0o755))
	mustErr(t, os.WriteFile(p, nil, 0o644))
	if FindClaudeTranscript(dir, "sid") != p || FindClaudeTranscript(dir, "zzz") != "" {
		t.Error("find failed")
	}
}

func write(t *testing.T, f *os.File, s string) {
	t.Helper()
	_, err := f.WriteString(s)
	mustErr(t, err)
}

func mustErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
