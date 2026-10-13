package usage

import "testing"

func TestModelIs(t *testing.T) {
	for _, c := range []struct {
		model, name string
		want        bool
	}{
		{"astra", "astra", true},
		{"ASTRA", "astra", true},
		{"gpt-6-astra", "astra", true},
		{"gpt-6-astra-2026-09", "Astra", true},
		{"console", "sol", false},
		{"gpt-6-luna", "astra", false},
		{"", "astra", false},
	} {
		if got := ModelIs(c.model, c.name); got != c.want {
			t.Errorf("ModelIs(%q, %q) = %v, want %v", c.model, c.name, got, c.want)
		}
	}
}

func TestCodexModelName(t *testing.T) {
	for model, want := range map[string]string{
		"gpt-6-luna": "luna", "terra": "terra", "gpt-6-sol-mini": "sol", "gpt-6-astra": "astra",
		"gpt-5-codex": "gpt-5-codex", CodexModel: "", "": "",
	} {
		if got := CodexModelName(model); got != want {
			t.Errorf("CodexModelName(%q) = %q, want %q", model, got, want)
		}
	}
}
