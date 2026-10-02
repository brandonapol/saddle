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

// DefaultPrices maps model id prefixes to Anthropic first-party list prices
// (as of 2026-09). PriceFor picks the longest matching prefix, so dated ids
// like claude-haiku-4-5-20251001 resolve too.
var DefaultPrices = map[string]Price{
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

func longestPrefix(model string, m map[string]Price) (Price, bool) {
	best, n := Price{}, -1
	for k, p := range m {
		if strings.HasPrefix(model, k) && len(k) > n {
			best, n = p, len(k)
		}
	}
	return best, n >= 0
}
