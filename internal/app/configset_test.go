package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/usage"
)

// #908: the orchestrator turns grok off with SetConfig instead of editing
// .saddle/config.toml by hand; the comments stay, this saddle applies it at
// once, and a typo or a bad value leaves the file as it was.
func TestSetConfig(t *testing.T) {
	a, _ := setup(t)
	write(t, a.Root, ".saddle/config.toml", "# mine\n[adapters]\npolicy = \"balanced\"\n")
	p := filepath.Join(a.Root, ".saddle", "config.toml")

	line, err := a.SetConfig("adapters.grok.disabled", "true")
	if err != nil || line != "disabled = true" {
		t.Fatalf("SetConfig = %q, %v", line, err)
	}
	if !a.Cfg.AdapterDisabled(usage.Grok) {
		t.Error("this saddle didn't apply the change")
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), "# mine") || !strings.Contains(string(b), "[adapters.grok]\ndisabled = true") {
		t.Errorf("file:\n%s", b)
	}
	if v, err := a.GetConfig("adapters.grok.disabled"); err != nil || v != "true" {
		t.Errorf("GetConfig = %q, %v", v, err)
	}
	if v, err := a.GetConfig("adapters.order"); err != nil || v != "" {
		t.Errorf("GetConfig(unset) = %q, %v", v, err)
	}

	for _, bad := range [][2]string{
		{"adapters.grok.disabeld", "true"}, // typo: no such setting
		{"adapters.policy", "fastest"},     // Load refuses it
		{"adapters.order", `["claude", "nope"]`},
	} {
		if _, err := a.SetConfig(bad[0], bad[1]); err == nil {
			t.Errorf("SetConfig(%s = %s) accepted", bad[0], bad[1])
		}
		if after, _ := os.ReadFile(p); string(after) != string(b) {
			t.Errorf("SetConfig(%s) changed the file:\n%s", bad[0], after)
		}
	}
}
