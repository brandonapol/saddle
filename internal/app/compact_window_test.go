package app

import "testing"

func TestContextWindowStripsProviderPrefix(t *testing.T) {
	cases := []struct {
		model string
		want  int64
	}{
		{"grok-4", 256_000},
		{"xai/grok-3", 131_072},
		{"openai/gpt-5-codex", 272_000},
		{"anthropic/claude-sonnet-4-5", 200_000},
		{"anthropic/claude-sonnet-4-5[1m]", longContext},
		{"opencode/big-pickle", DefaultContextWindow},
		{"openrouter/x-ai/grok-code-fast-1", 256_000},
		{"opencode/gpt-5", 272_000},
		{"unknown/model", DefaultContextWindow},
	}
	for _, c := range cases {
		if got := ContextWindow(c.model); got != c.want {
			t.Errorf("ContextWindow(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}
