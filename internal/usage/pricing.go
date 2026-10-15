package usage

import "strings"

// Price is a model's API list price in USD per million tokens. Subscription
// plans are not billed per token; Price only turns usage into a comparable
// $-equivalent.
type Price struct {
	Input      float64 `json:"input" toml:"input"`
	Output     float64 `json:"output" toml:"output"`
	CacheRead  float64 `json:"cache_read" toml:"cache_read"`
	CacheWrite float64 `json:"cache_write" toml:"cache_write"` // 5-minute TTL write rate
}

// Cost is the USD price of t at p. Transcripts do not split cache writes by
// TTL, so all of them are charged at the 5-minute rate.
func (p Price) Cost(t Tokens) float64 {
	return (float64(t.Input)*p.Input +
		float64(t.Output)*p.Output +
		float64(t.CacheRead)*p.CacheRead +
		float64(t.CacheCreation)*p.CacheWrite) / 1e6
}

// DefaultPrices maps model id prefixes to provider list prices (as of
// 2026-10). PriceFor picks the longest matching prefix, so dated ids
// like claude-haiku-4-5-20251001 resolve too.
//
// An entry named after an adapter (codex) is that adapter's default price:
// PriceForAdapter falls back to it for a model the table does not list, and
// [limits.prices.codex] overrides it.
var DefaultPrices = map[string]Price{
	Codex:               {Input: 1.75, Output: 14, CacheRead: 0.175}, // as gpt-5.3-codex
	"gpt-5.3-codex":     {Input: 1.75, Output: 14, CacheRead: 0.175},
	"gpt-5-codex":       {Input: 1.25, Output: 10, CacheRead: 0.125},
	"claude-fable-5-1":  {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	"claude-mythos-5-1": {Input: 10, Output: 50, CacheRead: 0.25, CacheWrite: 12.5},
	"claude-fable-5":    {Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	"claude-mythos-5":   {Input: 10, Output: 50, CacheRead: 1, CacheWrite: 12.5},
	"claude-opus-5-5":   {Input: 4, Output: 20, CacheRead: 0.2, CacheWrite: 5},
	"claude-opus-5":     {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-opus-4-8":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-opus-4-7":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-opus-4-6":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-opus-4-5":   {Input: 5, Output: 25, CacheRead: 0.5, CacheWrite: 6.25},
	"claude-opus-4":     {Input: 15, Output: 75, CacheRead: 1.5, CacheWrite: 18.75},
	"claude-sonnet-5-5": {Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5},
	"claude-sonnet-5":   {Input: 2, Output: 10, CacheRead: 0.2, CacheWrite: 2.5},
	"claude-sonnet-4":   {Input: 3, Output: 15, CacheRead: 0.3, CacheWrite: 3.75},
	"claude-haiku-4-5":  {Input: 1, Output: 5, CacheRead: 0.1, CacheWrite: 1.25},
}

// PriceFor returns the price for model. The longest matching prefix in
// overrides wins; failing that, the longest in DefaultPrices. ok is false if
// neither table knows the model.
func PriceFor(model string, overrides map[string]Price) (Price, bool) {
	if p, ok := longestPrefix(model, overrides); ok {
		return p, true
	}
	return longestPrefix(model, DefaultPrices)
}

// CostUSD is the $-equivalent of t on model, or 0 for an unpriced model.
func CostUSD(model string, t Tokens, overrides map[string]Price) float64 {
	p, _ := PriceFor(model, overrides)
	return p.Cost(t)
}

// PriceForAdapter is PriceFor for usage an adapter reported. A model the
// tables don't know is retried without its provider/ prefix (opencode
// labels models provider/model), then priced at the adapter's default entry,
// so new Codex models cost something rather than $0 (#637). ok is false only
// when none of these is priced.
func PriceForAdapter(adapter, model string, overrides map[string]Price) (Price, bool) {
	if p, ok := PriceFor(model, overrides); ok {
		return p, true
	}
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		if p, ok := PriceFor(model[i+1:], overrides); ok {
			return p, true
		}
	}
	if adapter == "" {
		return Price{}, false
	}
	if p, ok := overrides[adapter]; ok {
		return p, true
	}
	p, ok := DefaultPrices[adapter]
	return p, ok
}

// BucketCostUSD is b's $-equivalent: its provider-reported cost when it
// has one, else its tokens at PriceForAdapter. ok is false when neither is
// known, so callers can show the cost as unknown instead of $0.
func BucketCostUSD(b Bucket, overrides map[string]Price) (float64, bool) {
	return costOf(b.Adapter, b.Model, b.Tokens, b.CostUSD, overrides)
}

func costOf(adapter, model string, t Tokens, reported float64, overrides map[string]Price) (float64, bool) {
	if reported > 0 {
		return reported, true
	}
	p, ok := PriceForAdapter(adapter, model, overrides)
	return p.Cost(t), ok
}

func longestPrefix(model string, m map[string]Price) (Price, bool) {
	best, n := Price{}, -1
	for k, p := range m {
		if strings.HasPrefix(model, k) && len(k) > n {
			best, n = p, len(k)
		}
	}
	return best, n >= 0
}
