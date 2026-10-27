// Package agenttest fakes the CLIs saddle looks for on PATH (claude, codex,
// grok, opencode, gh, glab) and their logins, so setup checks run against a real
// PATH and home in tests (#646).
package agenttest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// creds is where each agent CLI keeps its login, relative to $HOME.
var creds = map[string]string{
	"claude": ".claude/.credentials.json",
	"codex":  ".codex/auth.json",
	"grok":   ".grok/auth.json",
	// XDG_DATA_HOME is cleared, so opencode's data dir is the default.
	"opencode": ".local/share/opencode/auth.json",
}

// authEnv is every variable that would count as a login, cleared so the
// developer's own keys don't leak into the fake machine.
var authEnv = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CONFIG_DIR", "OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_HOME", "XAI_API_KEY", "GROK_API_KEY",
	"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GENAI_USE_GCA", "OPENCODE_API_KEY",
	"OPENCODE_AUTH_CONTENT", "OPENROUTER_API_KEY", "GOOGLE_GENERATIVE_AI_API_KEY", "GROQ_API_KEY", "DEEPSEEK_API_KEY",
	"XDG_DATA_HOME", "GH_TOKEN", "GITHUB_TOKEN", "GITLAB_TOKEN", "SADDLE_HARNESS",
}

// Path makes a fresh $HOME and a PATH holding only bins, each a script that
// prints a version. Every name in logins is logged in: an agent CLI gets its
// credentials file, and gh or glab answers `auth status` with success. It
// returns the PATH dir. git, when installed, stays on PATH.
func Path(t *testing.T, bins, logins []string) string {
	t.Helper()
	dir, home := t.TempDir(), t.TempDir()
	if git, err := exec.LookPath("git"); err == nil {
		if err := os.Symlink(git, filepath.Join(dir, "git")); err != nil {
			t.Fatal(err)
		}
	}
	for _, k := range authEnv {
		t.Setenv(k, "")
	}
	t.Setenv("PATH", dir)
	t.Setenv("HOME", home)
	for _, b := range bins {
		in := slices.Contains(logins, b)
		if err := os.WriteFile(filepath.Join(dir, b), []byte(script(b, in)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, l := range logins {
		if c, ok := creds[l]; ok {
			p := filepath.Join(home, c)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return dir
}

func script(bin string, loggedIn bool) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\ncase \"$1\" in\n")
	switch bin {
	case "gh":
		b.WriteString("--version) echo 'gh version 2.101.0 (2026-09-15)'; echo 'https://github.com/cli/cli' ;;\n")
		if loggedIn {
			b.WriteString("auth) echo 'github.com'; echo '  - Logged in to github.com account me (keyring)'; echo \"  - Token scopes: 'repo', 'workflow'\" ;;\n")
		} else {
			b.WriteString("auth) echo 'You are not logged into any GitHub hosts. To log in, run: gh auth login' >&2; exit 1 ;;\n")
		}
	case "glab":
		b.WriteString("--version) echo 'glab 1.80.0 (2026-09-01)' ;;\n")
		if loggedIn {
			b.WriteString("auth) echo 'gitlab.com'; echo '  - Logged in to gitlab.com as me' ;;\n")
		} else {
			b.WriteString("auth) echo 'x gitlab.com: No token found' >&2; exit 1 ;;\n")
		}
	case "codex":
		b.WriteString("--version) echo 'codex-cli 1.0.0 (fake)' ;;\n")
		// saddle's codex orchestrator runs headless turns with exec --json (#668).
		b.WriteString("exec) echo 'Usage: codex exec [OPTIONS] [PROMPT]'; echo '      --json  Print events to stdout as JSONL' ;;\n")
	case "opencode":
		b.WriteString("--version) echo '1.19.0' ;;\n")
		// saddle's opencode orchestrator runs headless turns with run --format json (#745).
		b.WriteString("run) echo 'opencode run [message..]'; echo '      --format  format: default (formatted) or json (raw JSON events)' ;;\n")
	default:
		fmt.Fprintf(&b, "--version) echo '%s 1.0.0 (fake)' ;;\n", bin)
	}
	b.WriteString("esac\n")
	return b.String()
}
