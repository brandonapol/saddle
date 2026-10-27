package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/sso"
)

// #758: the SSO auth overrides reach both the interactive launch.sh and the
// headless bridge spec, and neither holds a token: codex runs saddle sso
// token for one when it needs it.
func TestCodexSSOArgsReachBothLaunches(t *testing.T) {
	args := sso.CodexArgs("/bin/saddle", "/repo", "corp", "corp-gw", "https://gw.example.com/v1")
	want := []string{`model_provider="corp-gw"`, `model_providers.corp-gw.auth.command="/bin/saddle"`,
		`model_providers.corp-gw.auth.args=["sso","token","--root","/repo","corp"]`, `model_providers.corp-gw.base_url="https://gw.example.com/v1"`}

	_, script, _ := launchScript(t, Codex{}, Launch{Bin: "/bin/saddle", Root: "/repo", Task: "t1", Dir: t.TempDir(), RunDir: t.TempDir(), Args: args})
	for _, w := range want {
		if !strings.Contains(script, w) {
			t.Errorf("launch.sh lacks %s:\n%s", w, script)
		}
	}

	run := t.TempDir()
	l := Launch{Kind: KindCodex, Bin: "/bin/saddle", Root: "/repo", Task: "orchestrator", Dir: "/repo", RunDir: run, Args: args}
	if _, err := l.Headless(""); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(run, "codex.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec codexSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(spec.Args, " ")
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("headless args lack %s: %s", w, got)
		}
	}
	for _, e := range spec.Env {
		if strings.Contains(strings.ToLower(e), "token") {
			t.Errorf("bridge env carries a token variable: %s", e)
		}
	}
}
