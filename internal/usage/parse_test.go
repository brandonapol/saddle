package usage

import (
	"path/filepath"
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
	}
	if total != (Tokens{Input: 800, CacheRead: 2900, Output: 130}) {
		t.Errorf("total = %+v", total)
	}
}
