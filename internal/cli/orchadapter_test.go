package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent/agenttest"
	"github.com/brandonapol/saddle/internal/config"
)

// #668: saddle init writes [orchestrator] adapter when the orchestrator
// can't run on Claude Code but another headless agent can, and keeps the
// owner's choice.
func TestInitSetupOrchestrator(t *testing.T) {
	const github = "git@github.com:o/r.git"
	for _, tc := range []struct {
		name    string
		cfg     string
		bins    []string
		adapter string // [orchestrator] adapter config.Load reads afterwards
		says    []string
		not     []string
	}{
		{"claude present", config.Template, []string{"gh", "claude", "codex"}, "",
			nil, []string{"orchestrator:"}},
		{"codex only, template", config.Template, []string{"gh", "codex"}, "codex",
			[]string{`wrote [orchestrator] adapter = "codex"`, "between turns"}, nil},
		{"codex only, no file", "", []string{"gh", "codex"}, "codex",
			[]string{`wrote [orchestrator] adapter = "codex"`}, nil},
		{"codex only, section without the line", "[orchestrator]\ncompact_at = 0.6\n\n[spawn]\nmax_depth = 2\n", []string{"gh", "codex"}, "codex",
			[]string{`wrote [orchestrator] adapter = "codex"`}, nil},
		{"grok only follows the harness", config.Template, []string{"gh", "grok"}, "",
			nil, []string{"orchestrator:"}},
		{"owner's choice kept", "[orchestrator]\nadapter = \"grok\"\n", []string{"gh", "codex", "grok"}, "grok",
			[]string{"orchestrator: grok ([orchestrator] adapter)"}, []string{"wrote [orchestrator]"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agenttest.Path(t, tc.bins, tc.bins)
			root := setupRepo(t, github, tc.cfg)
			if tc.cfg == "" {
				_ = os.Remove(filepath.Join(root, ".saddle", "config.toml"))
			}
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
				t.Fatalf("%v\n%s", err, readFile(t, filepath.Join(root, ".saddle", "config.toml")))
			}
			if cfg.Orchestrator.Adapter != tc.adapter {
				t.Errorf("orchestrator.adapter = %q, want %q\n%s", cfg.Orchestrator.Adapter, tc.adapter,
					readFile(t, filepath.Join(root, ".saddle", "config.toml")))
			}
			if tc.name == "codex only, section without the line" && (cfg.Orchestrator.CompactAt != 0.6 || cfg.Spawn.MaxDepth != 2) {
				t.Errorf("other keys lost: compact_at %v max_depth %d", cfg.Orchestrator.CompactAt, cfg.Spawn.MaxDepth)
			}
		})
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, _ := os.ReadFile(p)
	return string(b)
}
