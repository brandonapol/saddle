package config

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// #908: the orchestrator changes saddle's config with saddle config set,
// not a raw file edit. SetKey edits one line, keeps comments and the rest
// of the file, and creates the table when it is missing.
func TestSetKey(t *testing.T) {
	const in = "# saddle config\nharness = \"claude\" # the CLI\n\n[adapters]\n# rotation\norder = [\n  \"grok\",\n  \"claude\",\n]\npolicy = \"balanced\"\n\n[train]\nmax_attempts = 2\n"
	for _, c := range []struct {
		key, value string
		has        []string // substrings of the result
		check      func(map[string]any) bool
	}{
		{"adapters.order", `["claude", "codex"]`, []string{"# rotation", "policy = \"balanced\"", "# saddle config"},
			func(m map[string]any) bool {
				o := m["adapters"].(map[string]any)["order"].([]any)
				return len(o) == 2 && o[0] == "claude" && o[1] == "codex"
			}},
		{"adapters.grok.disabled", "true", []string{"[adapters.grok]\ndisabled = true"},
			func(m map[string]any) bool {
				return m["adapters"].(map[string]any)["grok"].(map[string]any)["disabled"] == true
			}},
		{"harness", "codex", []string{"harness = \"codex\""},
			func(m map[string]any) bool { return m["harness"] == "codex" }},
		{"train.max_attempts", "5", []string{"[train]\nmax_attempts = 5"},
			func(m map[string]any) bool { return m["train"].(map[string]any)["max_attempts"] == int64(5) }},
		{"train.lint.cmd", "make lint", nil,
			func(m map[string]any) bool {
				return m["train"].(map[string]any)["lint"].(map[string]any)["cmd"] == "make lint"
			}},
		{"test.cmd", `"go test ./..."`, []string{"[test]\ncmd = \"go test ./...\""},
			func(m map[string]any) bool { return m["test"].(map[string]any)["cmd"] == "go test ./..." }},
	} {
		out, err := SetKey(in, c.key, c.value)
		if err != nil {
			t.Errorf("SetKey(%s): %v", c.key, err)
			continue
		}
		var m map[string]any
		if _, err := toml.Decode(out, &m); err != nil {
			t.Errorf("SetKey(%s) wrote bad TOML: %v\n%s", c.key, err, out)
			continue
		}
		if !c.check(m) {
			t.Errorf("SetKey(%s) = wrong value:\n%s", c.key, out)
		}
		for _, h := range c.has {
			if !strings.Contains(out, h) {
				t.Errorf("SetKey(%s) lacks %q:\n%s", c.key, h, out)
			}
		}
	}
	if _, err := SetKey(in, "bad key", "1"); err == nil {
		t.Error("a key with a space was accepted")
	}
	if _, err := SetKey(in, "", "1"); err == nil {
		t.Error("an empty key was accepted")
	}
}

func TestUnknownKey(t *testing.T) {
	for key, unknown := range map[string]bool{
		"adapters.order":         false,
		"adapters.policy":        false,
		"adapters.grok.disabled": false,
		"train.max_attempts":     false,
		"harness":                false,
		"trian.max_attempts":     true,
		"adapters.grok.disabeld": true,
	} {
		out, err := SetKey("", key, "1")
		if err != nil {
			t.Fatal(err)
		}
		if got := UnknownKey(out, key); got != unknown {
			t.Errorf("UnknownKey(%s) = %v, want %v", key, got, unknown)
		}
	}
}
