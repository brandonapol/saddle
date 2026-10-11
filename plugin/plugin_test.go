// Package plugin holds the saddle Claude Code plugin. Its Go tests check the
// manifest and run the mod's own tests, so `make check` covers them (#166).
package plugin

import (
	"encoding/json"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"testing"
)

// hooks.json carries both the mod's hooks module and the settings hooks:
// adding the module must not drop the hooks older sessions rely on.
func TestHooksJSONKeepsSettingsHooksBesideModule(t *testing.T) {
	b, err := os.ReadFile("hooks/hooks.json")
	if err != nil {
		t.Fatal(err)
	}
	var h struct {
		Modules []string `json:"modules"`
		Hooks   map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &h); err != nil {
		t.Fatal(err)
	}
	if len(h.Modules) != 1 || h.Modules[0] != "./register.tsx" {
		t.Fatalf("modules = %v, want [./register.tsx]", h.Modules)
	}
	if _, err := os.Stat("hooks/register.tsx"); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop"} {
		found := false
		for _, m := range h.Hooks[ev] {
			for _, c := range m.Hooks {
				found = found || c.Command == "saddle plugin hook"
			}
		}
		if !found {
			t.Errorf("%s lost its `saddle plugin hook` settings hook", ev)
		}
	}
}

// plugin.json names the state contract the module's $.state keys are held to.
func TestPluginJSONNamesTypes(t *testing.T) {
	b, err := os.ReadFile(".claude-plugin/plugin.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Name  string `json:"name"`
		Types string `json:"types"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "saddle" || m.Types != "./types/index.d.ts" {
		t.Fatalf("plugin.json name %q types %q", m.Name, m.Types)
	}
	if _, err := os.Stat("types/index.d.ts"); err != nil {
		t.Fatal(err)
	}
}

// claudeWithMods returns the claude binary when it is new enough to load
// mods, skipping the test otherwise.
func claudeWithMods(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not installed")
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		t.Skipf("claude --version: %v", err)
	}
	v := regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`).FindStringSubmatch(string(out))
	if v == nil {
		t.Skipf("claude --version: %q", out)
	}
	have := [3]int{}
	for i := range have {
		have[i], _ = strconv.Atoi(v[i+1])
	}
	if want := [3]int{2, 1, 287}; have[0] < want[0] || have[0] == want[0] && (have[1] < want[1] || have[1] == want[1] && have[2] < want[2]) {
		t.Skipf("claude %s predates mods (2.1.287)", v[0])
	}
	return bin
}

// The engine accepts the mod: manifest, hooks module and state contract.
func TestModValidates(t *testing.T) {
	bin := claudeWithMods(t)
	if out, err := exec.Command(bin, "plugin", "validate", ".").CombinedOutput(); err != nil {
		t.Fatalf("claude plugin validate: %v\n%s", err, out)
	}
}

// The mod's own tests (tests/*.test.ts): snapshot parsing, the refresh
// timer and the pane's tabs, run by the engine without a live session.
func TestModTests(t *testing.T) {
	bin := claudeWithMods(t)
	if out, err := exec.Command(bin, "plugin", "test", ".").CombinedOutput(); err != nil {
		t.Fatalf("claude plugin test: %v\n%s", err, out)
	}
}
