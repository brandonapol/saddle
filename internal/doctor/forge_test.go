package doctor

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent/agenttest"
	"github.com/brandonapol/saddle/internal/config"
)

func TestRemoteHost(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:o/r.git":                "github.com",
		"https://github.com/o/r":                "github.com",
		"ssh://git@gitlab.example.com:2222/o/r": "gitlab.example.com",
		"https://user:tok@gitlab.com/g/s/r.git": "gitlab.com",
		"gitlab.com:o/r.git":                    "gitlab.com",
		"/srv/git/r.git":                        "",
		"":                                      "",
	} {
		if got := remoteHost(url); got != want {
			t.Errorf("remoteHost(%q) = %q, want %q", url, got, want)
		}
	}
}

// #646: fake gh and glab binaries on a fake PATH decide which CLI saddle
// uses for the origin remote's provider, falling back to the other CLI only
// when it is installed and logged in, and [forge] overrides detection.
func TestDetectForge(t *testing.T) {
	const gh, gl = "git@github.com:o/r.git", "git@gitlab.com:g/r.git"
	for _, tc := range []struct {
		name     string
		url      string
		bins     []string
		logins   []string
		override config.Forge
		provider string
		cli      string
		detail   string // substring of Detail
		fix      string // substring of Fix; "" means none
	}{
		{"github, gh logged in", gh, []string{"gh", "glab"}, []string{"gh", "glab"}, config.Forge{}, "github", "gh", "GitHub (github.com) via gh version 2.101.0", ""},
		{"github, gh missing", gh, nil, nil, config.Forge{}, "github", "", "gh is not installed", "https://cli.github.com"},
		{"github, gh logged out", gh, []string{"gh"}, nil, config.Forge{}, "github", "", "gh is not logged in to github.com", "gh auth login --hostname github.com"},
		{"github, gh logged out, glab logged in", gh, []string{"gh", "glab"}, []string{"glab"}, config.Forge{}, "github", "glab", "glab", ""},
		{"gitlab, glab logged in", gl, []string{"gh", "glab"}, []string{"gh", "glab"}, config.Forge{}, "gitlab", "glab", "GitLab detected, glab found, GitLab support is not available yet", ""},
		{"gitlab, glab missing", gl, nil, nil, config.Forge{}, "gitlab", "", "glab is not installed", "glab auth login --hostname gitlab.com"},
		{"gitlab, glab missing, gh logged in", gl, []string{"gh"}, []string{"gh"}, config.Forge{}, "gitlab", "gh", "via gh", ""},
		{"unknown host prefers gh", "git@git.corp:o/r.git", []string{"gh", "glab"}, []string{"gh", "glab"}, config.Forge{}, "github", "gh", "via gh", ""},
		{"override cli none", gh, []string{"gh"}, []string{"gh"}, config.Forge{CLI: "none"}, "github", "", `cli = "none"`, ""},
		{"override provider", "git@git.corp:o/r.git", []string{"gh", "glab"}, []string{"gh", "glab"}, config.Forge{Provider: "gitlab"}, "gitlab", "glab", "GitLab detected", ""},
		{"override cli without fallback", gh, []string{"gh", "glab"}, []string{"gh"}, config.Forge{CLI: "glab"}, "github", "", "glab is not logged in to github.com", "glab auth login --hostname github.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agenttest.Path(t, tc.bins, tc.logins)
			f := DetectForge(System(t.TempDir()), tc.url, tc.override)
			if f.Provider != tc.provider || f.CLI != tc.cli {
				t.Errorf("provider, cli = %q, %q; want %q, %q (%+v)", f.Provider, f.CLI, tc.provider, tc.cli, f)
			}
			if !strings.Contains(f.Detail, tc.detail) {
				t.Errorf("detail %q lacks %q", f.Detail, tc.detail)
			}
			if tc.fix == "" && f.CLI == "gh" && f.Fix != "" {
				t.Errorf("fix %q, want none", f.Fix)
			}
			if !strings.Contains(f.Fix, tc.fix) {
				t.Errorf("fix %q lacks %q", f.Fix, tc.fix)
			}
			if f.CLI != "gh" && !strings.Contains(f.Detail, "local-only") {
				t.Errorf("detail %q doesn't say saddle runs local-only", f.Detail)
			}
		})
	}
}
