package cli

import (
	"strings"
	"testing"
)

// #908: saddle config set changes a setting without a raw file edit, and
// saddle config get reads it back.
func TestConfigCommand(t *testing.T) {
	a := automergeRepo(t)
	t.Chdir(a.Root)
	if out := run(t, "config", "get", "adapters.grok.disabled"); !strings.Contains(out, "not set") {
		t.Fatalf("get unset:\n%s", out)
	}
	if out := run(t, "config", "set", "adapters.grok.disabled", "true"); !strings.Contains(out, "disabled = true") {
		t.Fatalf("set:\n%s", out)
	}
	if out := run(t, "config", "get", "adapters.grok.disabled"); strings.TrimSpace(out) != "true" {
		t.Fatalf("get:\n%s", out)
	}
	if out := run(t, "config", "set", "train.lint.cmd", "make", "lint"); !strings.Contains(out, `"make lint"`) {
		t.Fatalf("set words:\n%s", out)
	}
}
