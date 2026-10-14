package usage

import (
	"os"
	"path/filepath"
	"reflect"
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
