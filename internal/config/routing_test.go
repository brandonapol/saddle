package config

import (
	"strings"
	"testing"
)

// #505: [adapters.<name>] carries a capability table: the difficulties it
// suits, its capabilities and a model per difficulty. Unset keys take the
// built-in defaults; excluded models are never returned.
func TestAdapterCapabilityTable(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SuitsDifficulty("claude", DifficultyHard) || cfg.SuitsDifficulty("codex", DifficultyHard) || !cfg.SuitsDifficulty("codex", "") {
		t.Fatalf("default difficulties: claude %v, codex %v", cfg.AdapterDifficulties("claude"), cfg.AdapterDifficulties("codex"))
	}
	if got := cfg.DifficultyModel("claude", DifficultyHard); got != "opus" {
		t.Fatalf("claude hard model = %q", got)
	}
	if got := cfg.DifficultyModel("codex", DifficultyMechanical); got != "" {
		t.Fatalf("codex mechanical model = %q, want its default", got)
	}

	root := t.TempDir()
	writeConfig(t, root, "[adapters.codex]\ndifficulties = [\"hard-design\", \"Standard\"]\ncapabilities = [\"Image-Gen\", \"hooks-required\"]\n"+
		"exclude_models = [\"astra\"]\n[adapters.codex.models]\nstandard = \"sol\"\nmechanical = \"luna\"\nhard = \"gpt-6-astra\"\n")
	if cfg, err = Load(root); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.AdapterDifficulties("codex"), ","); got != "hard,standard" {
		t.Fatalf("difficulties = %s", got)
	}
	if got := strings.Join(cfg.AdapterCapabilities("codex"), ","); got != "image-gen,hooks" {
		t.Fatalf("capabilities = %s", got)
	}
	if got := cfg.DifficultyModel("codex", DifficultyStandard); got != "sol" {
		t.Fatalf("standard model = %q", got)
	}
	if got := cfg.DifficultyModel("codex", DifficultyHard); got != "" {
		t.Fatalf("excluded hard model returned: %q", got)
	}
	for _, bad := range []string{"[adapters.codex]\ndifficulties = [\"impossible\"]\n", "[adapters.codex]\nmodels = \"sol\"\n", "[adapters.codex.models]\nepic = \"sol\"\n"} {
		writeConfig(t, root, bad)
		if _, err := Load(root); err == nil {
			t.Errorf("%q loaded", bad)
		}
	}
}

func TestNormalizeDifficulty(t *testing.T) {
	for in, want := range map[string]string{
		"hard-design": "hard", "Hard (design judgment).": "hard", "**hard**": "hard", "standard": "standard",
		"Mechanical": "mechanical", "easy": "mechanical", "whatever": "",
	} {
		if got := NormalizeDifficulty(in); got != want {
			t.Errorf("NormalizeDifficulty(%q) = %q, want %q", in, got, want)
		}
	}
}
