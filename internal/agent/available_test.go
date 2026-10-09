package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeMachine(t *testing.T, onPath []string, env map[string]string) machine {
	t.Helper()
	home := t.TempDir()
	return machine{
		home: home,
		goos: "linux",
		getenv: func(k string) string {
			return env[k]
		},
		lookPath: func(cmd string) (string, error) {
			for _, p := range onPath {
				if p == cmd {
					return "/usr/bin/" + cmd, nil
				}
			}
			return "", errors.New("not found")
		},
	}
}

func touch(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestAvailabilityNeedsBinaryAndAuth(t *testing.T) {
	m := fakeMachine(t, []string{"claude", "codex", "grok"}, map[string]string{"GEMINI_API_KEY": "k"})
	touch(t, filepath.Join(m.home, ".claude", ".credentials.json"))
	touch(t, filepath.Join(m.home, ".grok", "auth.json"))
	got := map[string]Status{}
	for _, s := range m.availability(nil) {
		got[s.Name] = s
	}
	if len(got) != len(Names()) {
		t.Fatalf("availability lists %d adapters, want %d: %+v", len(got), len(Names()), got)
	}
	if !got["claude"].OK || !got["grok"].OK {
		t.Errorf("claude and grok are installed and logged in: %+v", got)
	}
	if s := got["codex"]; s.OK || !strings.Contains(s.Reason, "codex login") {
		t.Errorf("codex has no auth: %+v", s)
	}
	if s := got["gemini"]; s.OK || !strings.Contains(s.Reason, "not on PATH") {
		t.Errorf("gemini is not installed: %+v", s)
	}
}

func TestAvailabilityUsesConfiguredCmd(t *testing.T) {
	m := fakeMachine(t, []string{"my-codex"}, map[string]string{"OPENAI_API_KEY": "k"})
	if err := m.check("codex", ""); err == nil {
		t.Error("codex is not on PATH under its default name")
	}
	if err := m.check("codex", "my-codex"); err != nil {
		t.Errorf("[adapters.codex] cmd is on PATH: %v", err)
	}
}

func TestAvailabilityAuthFiles(t *testing.T) {
	m := fakeMachine(t, []string{"codex", "gemini", "claude"}, map[string]string{"CODEX_HOME": "", "CLAUDE_CONFIG_DIR": ""})
	touch(t, filepath.Join(m.home, ".codex", "auth.json"))
	touch(t, filepath.Join(m.home, ".gemini", "oauth_creds.json"))
	for _, n := range []string{"codex", "gemini"} {
		if err := m.check(n, ""); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
	if err := m.check("claude", ""); err == nil {
		t.Error("claude has no credentials")
	}
	m.goos = "darwin" // credentials live in the keychain
	if err := m.check("claude", ""); err != nil {
		t.Errorf("darwin claude: %v", err)
	}
	if err := m.check("nope", ""); err == nil {
		t.Error("unknown adapter")
	}
}

func TestUnavailableErrListsUsable(t *testing.T) {
	err := Unavailable("gemini", errors.New("gemini not on PATH"), []Status{
		{Name: "claude", OK: true}, {Name: "gemini", Reason: "gemini not on PATH"}, {Name: "grok", OK: true},
	})
	for _, want := range []string{"gemini not on PATH", "claude, grok"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%v lacks %q", err, want)
		}
	}
}
