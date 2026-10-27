package agent

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent/agenttest"
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

// #646: the harness falls back to the first usable one in order when the
// configured one can't run; none usable is reported, not guessed.
func TestFallbackHarness(t *testing.T) {
	order := []string{"claude", "grok", "codex"}
	st := func(ok ...string) []Status {
		var ss []Status
		for _, n := range order {
			ss = append(ss, Status{Name: n, OK: slices.Contains(ok, n)})
		}
		return ss
	}
	for _, tc := range []struct {
		want string
		ss   []Status
		got  string
		ok   bool
	}{
		{"claude", st("claude", "codex"), "claude", true},
		{"claude", st("codex"), "codex", true},
		{"claude", st("grok", "codex"), "grok", true},
		{"codex", st("claude", "codex"), "codex", true},
		{"codex", st("claude"), "claude", true},
		{"claude", st(), "", false},
	} {
		got, ok := FallbackHarness(tc.want, order, tc.ss)
		if got != tc.got || ok != tc.ok {
			t.Errorf("FallbackHarness(%s, %+v) = %q, %v; want %q, %v", tc.want, tc.ss, got, ok, tc.got, tc.ok)
		}
	}
}

func TestInstallSteps(t *testing.T) {
	s := InstallSteps([]string{"claude", "codex"})
	for _, want := range []string{"https://claude.com/claude-code", "claude /login", "@openai/codex", "codex login"} {
		if !strings.Contains(s, want) {
			t.Errorf("install steps lack %q:\n%s", want, s)
		}
	}
}

// #646: fake claude and codex binaries on a fake PATH, with or without their
// logins, decide which harness is usable on the real machine checks.
func TestAvailabilityOnFakePath(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bins   []string
		logins []string
		usable []string
	}{
		{"claude only", []string{"claude"}, []string{"claude"}, []string{"claude"}},
		{"claude missing, codex present", []string{"codex"}, []string{"codex"}, []string{"codex"}},
		{"claude logged out, codex present", []string{"claude", "codex"}, []string{"codex"}, []string{"codex"}},
		{"both", []string{"claude", "codex"}, []string{"claude", "codex"}, []string{"claude", "codex"}},
		{"nothing", nil, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agenttest.Path(t, tc.bins, tc.logins)
			ss := Availability(nil)
			if got := Usable(ss); !slices.Equal(got, tc.usable) {
				t.Errorf("usable = %v, want %v", got, tc.usable)
			}
			for _, s := range ss {
				if s.Installed != slices.Contains(tc.bins, s.Name) {
					t.Errorf("%s installed = %v, want %v", s.Name, s.Installed, !s.Installed)
				}
			}
		})
	}
}

// A CLI that is installed but has no credentials file saddle knows may
// still be logged in (codex's keyring store, an apiKeyHelper). When no CLI
// is verified, fall back to the first installed one rather than to a
// harness that isn't installed at all.
func TestFallbackHarnessTakesInstalledWhenNoneVerified(t *testing.T) {
	ss := []Status{
		{Name: "claude", Reason: "claude not on PATH"},
		{Name: "grok", Reason: "grok not on PATH"},
		{Name: "codex", Installed: true, Reason: "codex is not logged in: run codex login"},
	}
	if got, ok := FallbackHarness("claude", []string{"claude", "grok", "codex"}, ss); got != "codex" || !ok {
		t.Errorf("FallbackHarness = %q, %v; want codex, true", got, ok)
	}
	ss[0].OK, ss[0].Installed = true, true
	if got, _ := FallbackHarness("codex", []string{"claude", "grok", "codex"}, ss); got != "claude" {
		t.Errorf("a verified CLI must win over an unverified one: got %q", got)
	}
}
