package usage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func pollCodex(t *testing.T, path string) []Bucket {
	t.Helper()
	c := NewCollector()
	if !c.Track(Session{ID: "codex:t1", Task: "t1", Agent: Codex, Path: path}) {
		t.Fatal("codex not tracked")
	}
	bs, err := c.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) == 0 {
		t.Fatal("no buckets")
	}
	return bs
}

// #637: a rollout whose turn_context names a priced model costs its usage at
// that model's table price.
func TestCodexRolloutWithModelLineCostsTablePrice(t *testing.T) {
	bs := pollCodex(t, filepath.Join("testdata", "codex-rollout.jsonl"))
	var got, want float64
	for _, b := range bs {
		cost, ok := BucketCostUSD(b, nil)
		if !ok {
			t.Errorf("bucket %+v unpriced", b)
		}
		got += cost
		want += DefaultPrices["gpt-5-codex"].Cost(b.Tokens)
	}
	if got <= 0 || !near(got, want) {
		t.Errorf("cost = %v, want %v", got, want)
	}
}

// #637: a rollout with no model line falls back to the default Codex price,
// not $0.
func TestCodexRolloutWithoutModelLineUsesDefaultCodexPrice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	data := `{"timestamp":"2026-10-02T10:00:00.000Z","type":"session_meta","payload":{"id":"0199a","cwd":"/repo/wt"}}
{"timestamp":"2026-10-02T10:00:09.000Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":1200,"cached_input_tokens":1000,"output_tokens":80,"total_tokens":1280},"last_token_usage":{"input_tokens":1200,"cached_input_tokens":1000,"output_tokens":80,"total_tokens":1280}}}}
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	bs := pollCodex(t, path)
	cost, ok := BucketCostUSD(bs[0], nil)
	want := DefaultPrices[Codex].Cost(Tokens{Input: 200, CacheRead: 1000, Output: 80})
	if !ok || cost <= 0 || !near(cost, want) {
		t.Errorf("cost = %v ok=%v, want %v", cost, ok, want)
	}
}

// #637: today's rollouts name models the table doesn't list (gpt-5.6-sol).
// Codex usage on such a model costs the default Codex price, which
// [limits.prices.codex] overrides.
func TestUnknownCodexModelUsesDefaultCodexPrice(t *testing.T) {
	b := Bucket{Key: Key{Adapter: Codex, Model: "gpt-5.6-sol"}, Tokens: Tokens{Input: 1_000_000, Output: 1_000_000}}
	cost, ok := BucketCostUSD(b, nil)
	if want := DefaultPrices[Codex].Cost(b.Tokens); !ok || cost <= 0 || !near(cost, want) {
		t.Errorf("default: cost = %v ok=%v, want %v", cost, ok, want)
	}
	cost, ok = BucketCostUSD(b, map[string]Price{Codex: {Input: 1, Output: 2}})
	if !ok || !near(cost, 3) {
		t.Errorf("override: cost = %v ok=%v, want 3", cost, ok)
	}
	// A listed model still wins over the fallback.
	b.Model = "gpt-5-codex"
	if cost, _ := BucketCostUSD(b, map[string]Price{Codex: {Input: 1, Output: 2}}); !near(cost, 11.25) {
		t.Errorf("listed model cost = %v, want 11.25", cost)
	}
}

// #637: grok has no default price, so its usage stays visibly unpriced
// unless reported by the provider or configured.
func TestGrokUsageUnpricedUnlessConfigured(t *testing.T) {
	b := Bucket{Key: Key{Adapter: Grok, Model: "grok-4.7-build"}, Tokens: Tokens{Input: 1_000_000}}
	if cost, ok := BucketCostUSD(b, nil); ok || cost != 0 {
		t.Errorf("grok priced without a price: %v %v", cost, ok)
	}
	if cost, ok := BucketCostUSD(b, map[string]Price{Grok: {Input: 2}}); !ok || !near(cost, 2) {
		t.Errorf("grok override: %v %v", cost, ok)
	}
	b.CostUSD = 0.5
	if cost, ok := BucketCostUSD(b, nil); !ok || !near(cost, 0.5) {
		t.Errorf("reported grok cost: %v %v", cost, ok)
	}
}

// #637: opencode labels models provider/model; the model part is priced.
func TestOpenCodeProviderModelPriced(t *testing.T) {
	b := Bucket{Key: Key{Adapter: OpenCode, Model: "openai/gpt-5-codex"}, Tokens: Tokens{Input: 1_000_000}}
	if cost, ok := BucketCostUSD(b, nil); !ok || !near(cost, 1.25) {
		t.Errorf("opencode cost = %v ok=%v, want 1.25", cost, ok)
	}
}

// #637: plan-limit windows price unknown Codex models at the default too,
// and don't list them as unpriced.
func TestEstimateBucketsPricesUnknownCodexModel(t *testing.T) {
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	bs := []Bucket{
		{Key: Key{Minute: now, Adapter: Codex, Model: "gpt-5.6-sol"}, Tokens: Tokens{Input: 1_000_000}, Messages: 1},
		{Key: Key{Minute: now, Adapter: Grok, Model: "grok-4.7-build"}, Tokens: Tokens{Input: 1_000_000}, Messages: 1},
	}
	e := EstimateBuckets(bs, Limits{}, now)
	if want := DefaultPrices[Codex].Input; !near(e.FiveHour.USD, want) {
		t.Errorf("usd = %v, want %v", e.FiveHour.USD, want)
	}
	if len(e.FiveHour.Unpriced) != 1 || e.FiveHour.Unpriced[0] != "grok-4.7-build" {
		t.Errorf("unpriced = %v", e.FiveHour.Unpriced)
	}
}
