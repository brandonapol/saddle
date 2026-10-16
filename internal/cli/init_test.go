package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent/agenttest"
	"github.com/brandonapol/saddle/internal/config"
)

// setupRepo is a git repo with origin at remote and saddle's config template,
// as saddle init leaves it.
func setupRepo(t *testing.T, remote, cfg string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	root := t.TempDir()
	gitRun(t, root, "init", "-q", "-b", "main")
	gitRun(t, root, "remote", "add", "origin", remote)
	if err := os.MkdirAll(filepath.Join(root, ".saddle"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".saddle", "config.toml"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// #646: saddle init says which git provider CLI and agent CLIs it found,
// makes the first usable agent the default when claude can't run, and exits
// cleanly with install steps when no agent CLI can.
func TestInitSetup(t *testing.T) {
	const github, gitlab = "git@github.com:o/r.git", "git@gitlab.com:g/r.git"
	for _, tc := range []struct {
		name    string
		remote  string
		cfg     string
		bins    []string
		harness string   // harness config.Load reads afterwards
		says    []string // substrings of the output
		not     []string
		logins  []string // nil: every bin is logged in
	}{
		{"gh and claude", github, config.Template, []string{"gh", "claude"}, "claude",
			[]string{"git provider: GitHub (github.com) via gh", "agent: claude"}, []string{"wrote harness", "fix:"}, nil},
		{"glab only", gitlab, config.Template, []string{"glab", "claude"}, "claude",
			[]string{"GitLab detected, glab found, GitLab support is not available yet", "local-only"}, nil, nil},
		{"no provider CLI", github, config.Template, []string{"claude"}, "claude",
			[]string{"local-only", "fix: install the GitHub CLI (https://cli.github.com), then gh auth login --hostname github.com"}, nil, nil},
		{"claude missing, codex present", github, config.Template, []string{"gh", "codex"}, "codex",
			[]string{"claude can't run (claude not on PATH)", `wrote harness = "codex" to .saddle/config.toml`, "Codex has no hooks, so claims are checked only at commit and done"}, nil, nil},
		{"claude missing, grok present", github, config.Template, []string{"gh", "grok"}, "grok",
			[]string{`wrote harness = "grok"`}, []string{"no hooks"}, nil},
		{"harness set by the owner is kept", github, "harness = \"claude\"\n", []string{"gh", "codex"}, "claude",
			[]string{`set harness = "codex" in .saddle/config.toml`}, []string{"wrote harness"}, nil},
		{"claude looks logged out, nothing else", github, config.Template, []string{"gh", "claude"}, "claude",
			[]string{"agent: claude looks logged out (claude is not logged in: run claude /login)"}, []string{"wrote harness", "no agent CLI"}, []string{"gh"}},
		{"nothing present", github, config.Template, nil, "claude",
			[]string{"no agent CLI is installed and logged in", "@anthropic-ai/claude-code", "codex login", "run saddle init again"}, []string{"wrote harness"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logins := tc.logins
			if logins == nil {
				logins = tc.bins
			}
			agenttest.Path(t, tc.bins, logins)
			root := setupRepo(t, tc.remote, tc.cfg)
			var out strings.Builder
			if err := initSetup(&out, root); err != nil {
				t.Fatalf("initSetup: %v\n%s", err, out.String())
			}
			s := out.String()
			for _, w := range tc.says {
				if !strings.Contains(s, w) {
					t.Errorf("output lacks %q:\n%s", w, s)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(s, w) {
					t.Errorf("output has %q:\n%s", w, s)
				}
			}
			cfg, err := config.Load(root)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Harness != tc.harness {
				t.Errorf("harness = %q, want %q", cfg.Harness, tc.harness)
			}
		})
	}
}
