package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Status says whether an adapter can run on this machine (#182).
type Status struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"` // why not, when !OK
}

// auth is how an adapter's CLI finds its credentials: any of these
// environment variables, or any of these files under its home.
type auth struct {
	bin      string   // default binary
	env      []string // API keys or switches the CLI accepts
	homeEnv  string   // overrides the home dir below
	home     string   // relative to the user's home
	files    []string // relative to home; any one is enough
	login    string   // how the user logs in
	keychain bool     // on darwin credentials are in the keychain, not a file
}

var auths = map[string]auth{
	"claude": {bin: "claude", env: []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX"},
		homeEnv: "CLAUDE_CONFIG_DIR", home: ".claude", files: []string{".credentials.json"}, login: "claude /login", keychain: true},
	"codex": {bin: "codex", env: []string{"OPENAI_API_KEY", "CODEX_API_KEY"},
		homeEnv: "CODEX_HOME", home: ".codex", files: []string{"auth.json"}, login: "codex login"},
	GeminiName: {bin: "gemini", env: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GENAI_USE_GCA"},
		home: ".gemini", files: []string{"oauth_creds.json"}, login: "gemini (sign in), or set GEMINI_API_KEY"},
	"grok": {bin: "grok", env: []string{"XAI_API_KEY", "GROK_API_KEY"},
		home: ".grok", files: []string{"auth.json"}, login: "grok login"},
}

// machine is what availability looks at, so tests can fake it.
type machine struct {
	home     string
	goos     string
	getenv   func(string) string
	lookPath func(string) (string, error)
}

func thisMachine() machine {
	h, _ := os.UserHomeDir()
	return machine{home: h, goos: runtime.GOOS, getenv: os.Getenv, lookPath: exec.LookPath}
}

// Check returns why adapter name can't run here, or nil. cmd is its
// configured binary; empty means the CLI's own name. The auth check is cheap:
// an API key in the environment or the CLI's credentials file.
func Check(name, cmd string) error { return thisMachine().check(name, cmd) }

// Availability lists every adapter and whether it can run here. cmds maps an
// adapter to its configured binary.
func Availability(cmds map[string]string) []Status { return thisMachine().availability(cmds) }

// Usable is the names of the adapters in ss that can run.
func Usable(ss []Status) []string {
	var out []string
	for _, s := range ss {
		if s.OK {
			out = append(out, s.Name)
		}
	}
	return out
}

// Unavailable is the spawn error for an adapter that can't run: the reason
// and the adapters that can.
func Unavailable(name string, why error, ss []Status) error {
	ok := "none"
	if u := Usable(ss); len(u) > 0 {
		ok = strings.Join(u, ", ")
	}
	return fmt.Errorf("adapter %s is not available: %v (available: %s)", name, why, ok)
}

func (m machine) availability(cmds map[string]string) []Status {
	var out []Status
	for _, n := range Names() {
		s := Status{Name: n, OK: true}
		if err := m.check(n, cmds[n]); err != nil {
			s.OK, s.Reason = false, err.Error()
		}
		out = append(out, s)
	}
	return out
}

func (m machine) check(name, cmd string) error {
	a, ok := auths[name]
	if !ok {
		return fmt.Errorf("unknown adapter %q: want one of %s", name, strings.Join(Names(), ", "))
	}
	if cmd == "" {
		cmd = a.bin
	}
	if _, err := m.lookPath(cmd); err != nil {
		return fmt.Errorf("%s not on PATH", cmd)
	}
	for _, k := range a.env {
		if m.getenv(k) != "" {
			return nil
		}
	}
	if a.keychain && m.goos == "darwin" {
		return nil
	}
	home := m.getenv(a.homeEnv)
	if a.homeEnv == "" || home == "" {
		home = filepath.Join(m.home, a.home)
	}
	for _, f := range a.files {
		if _, err := os.Stat(filepath.Join(home, f)); err == nil {
			return nil
		}
	}
	return fmt.Errorf("%s is not logged in: run %s", name, a.login)
}
