package config

import (
	"strings"
	"testing"
)

// #839: [adapters.<name>] disabled = true turns an adapter off for good;
// a non-boolean is refused.
func TestAdapterDisabled(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	writeConfig(t, root, "[adapters.grok]\ndisabled = true\n\n[adapters.codex]\ndisabled = false\n")
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AdapterDisabled("grok") {
		t.Error("grok not disabled")
	}
	if cfg.AdapterDisabled("codex") || cfg.AdapterDisabled("claude") {
		t.Error("codex or claude disabled")
	}
	if got := strings.Join(cfg.DisabledAdapters(), ","); got != "grok" {
		t.Errorf("DisabledAdapters = %q, want grok", got)
	}
	bad := t.TempDir()
	writeConfig(t, bad, "[adapters.grok]\ndisabled = \"yes\"\n")
	if _, err := Load(bad); err == nil || !strings.Contains(err.Error(), "adapters.grok.disabled") {
		t.Errorf("non-boolean disabled: err = %v", err)
	}
}
