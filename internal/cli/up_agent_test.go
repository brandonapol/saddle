package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent"
)

// #150: saddle up [agent] [epic-file|-].
func TestParseUpArgs(t *testing.T) {
	dir := t.TempDir()
	epic := filepath.Join(dir, "epic.md")
	if err := os.WriteFile(epic, []byte("do it"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	// A file literally named after an agent: the bare word is the agent.
	if err := os.WriteFile("grok", []byte("epic"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		args          []string
		harness, epic string
	}{
		{nil, "", ""},
		{[]string{"claude"}, "claude", ""},
		{[]string{"grok"}, "grok", ""},
		{[]string{"codex"}, "codex", ""},
		{[]string{"grok", epic}, "grok", epic},
		{[]string{"claude", "-"}, "claude", "-"},
		{[]string{"-"}, "", "-"},
		{[]string{epic}, "", epic},
		{[]string{"./grok"}, "", "./grok"},
		{[]string{"claude", "./grok"}, "claude", "./grok"},
	} {
		h, e, err := parseUpArgs(c.args)
		if err != nil || h != c.harness || e != c.epic {
			t.Errorf("parseUpArgs(%q) = %q, %q, %v; want %q, %q", c.args, h, e, err, c.harness, c.epic)
		}
	}
	for _, bad := range [][]string{{"cursor"}, {epic, "-"}, {"grok", "codex", "-"}} {
		if _, _, err := parseUpArgs(bad); err == nil {
			t.Errorf("parseUpArgs(%q) should fail", bad)
		} else if bad[0] == "cursor" && !strings.Contains(err.Error(), "claude, codex, grok") {
			t.Errorf("unknown word should list the agents: %v", err)
		}
	}
}

// The chosen agent applies to this process tree only: it is set in the
// environment config.Load reads, and the config file is untouched.
func TestUpAgentSetsHarnessEnv(t *testing.T) {
	t.Setenv("SADDLE_HARNESS", "")
	if err := applyUpHarness("codex"); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("SADDLE_HARNESS"); got != "codex" {
		t.Errorf("SADDLE_HARNESS = %q", got)
	}
	t.Setenv("SADDLE_HARNESS", "grok")
	if err := applyUpHarness(""); err != nil || os.Getenv("SADDLE_HARNESS") != "grok" {
		t.Errorf("bare up must leave the inherited harness alone: %q %v", os.Getenv("SADDLE_HARNESS"), err)
	}
}

func TestWriteAdapters(t *testing.T) {
	var b strings.Builder
	writeAdapters(&b, []agent.Status{{Name: "claude", OK: true}, {Name: "gemini", Reason: "gemini not on PATH"}, {Name: "grok", OK: true}})
	if got := b.String(); got != "adapters: claude, grok; gemini unavailable (gemini not on PATH)\n" {
		t.Errorf("got %q", got)
	}
}
