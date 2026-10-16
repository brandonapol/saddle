package doctor

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent/agenttest"
	"github.com/brandonapol/saddle/internal/config"
)

// #646: fake gh and glab binaries on a fake PATH decide which CLI saddle
// uses for the origin remote's provider. #645: each provider takes its own
// CLI; only an unknown host tries both, and [forge] overrides detection.
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
		// #645: glab can't drive GitHub, nor gh GitLab: no fallback once the host names the provider.
		{"github, gh logged out, glab logged in", gh, []string{"gh", "glab"}, []string{"glab"}, config.Forge{}, "github", "", "gh is not logged in to github.com", "gh auth login --hostname github.com"},
		{"gitlab, glab logged in", gl, []string{"gh", "glab"}, []string{"gh", "glab"}, config.Forge{}, "gitlab", "glab", "GitLab (gitlab.com) via glab 1.80.0", ""},
		{"gitlab, glab missing", gl, nil, nil, config.Forge{}, "gitlab", "", "glab is not installed", "glab auth login --hostname gitlab.com"},
		{"gitlab, glab missing, gh logged in", gl, []string{"gh"}, []string{"gh"}, config.Forge{}, "gitlab", "", "glab is not installed", "https://gitlab.com/gitlab-org/cli"},
		{"gitlab, glab logged out", gl, []string{"glab"}, nil, config.Forge{}, "gitlab", "", "glab is not logged in to gitlab.com", "glab auth login --hostname gitlab.com"},
		{"unknown host prefers gh", "git@git.corp:o/r.git", []string{"gh", "glab"}, []string{"gh", "glab"}, config.Forge{}, "github", "gh", "via gh", ""},
		{"unknown host, only glab", "git@git.corp:o/r.git", []string{"glab"}, []string{"glab"}, config.Forge{}, "gitlab", "", "nothing says the repo is on GitLab", `provider = "gitlab"`},
		{"override cli none", gh, []string{"gh"}, []string{"gh"}, config.Forge{CLI: "none"}, "github", "", `cli = "none"`, ""},
		{"override provider", "git@git.corp:o/r.git", []string{"gh", "glab"}, []string{"gh", "glab"}, config.Forge{Provider: "gitlab"}, "gitlab", "glab", "GitLab (git.corp) via glab", ""},
		{"override provider and host", "git@git.corp:o/r.git", []string{"glab"}, []string{"glab"}, config.Forge{Provider: "gitlab", Host: "gitlab.corp.example"}, "gitlab", "glab", "GitLab (gitlab.corp.example) via glab", ""},
		{"override cli glab names gitlab", gh, []string{"gh", "glab"}, []string{"gh"}, config.Forge{CLI: "glab"}, "gitlab", "", "glab is not logged in to github.com", "glab auth login --hostname github.com"},
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
			if tc.fix == "" && f.CLI != "" && f.Fix != "" {
				t.Errorf("fix %q, want none", f.Fix)
			}
			if !strings.Contains(f.Fix, tc.fix) {
				t.Errorf("fix %q lacks %q", f.Fix, tc.fix)
			}
			if f.CLI == "" && !strings.Contains(f.Detail, "local-only") {
				t.Errorf("detail %q doesn't say saddle runs local-only", f.Detail)
			}
			if f.LocalOnly() != (f.CLI == "") || strings.Contains(f.Detail, "not available yet") {
				t.Errorf("detail %q, cli %q: GitLab through glab is supported now (#645)", f.Detail, f.CLI)
			}
		})
	}
}

// #645: on GitLab through glab the provider check passes, and the checks
// that still read GitHub's API through gh (#648) say they are skipped there
// instead of blaming the provider.
func TestDoctorOnGitLab(t *testing.T) {
	agenttest.Path(t, []string{"glab", "claude"}, []string{"glab", "claude"})
	f := healthy(t)
	f.git["remote get-url origin"] = res{out: "git@gitlab.com:g/r.git"}
	got := map[string]Result{}
	for _, r := range Run(pathEnv{f, System(f.root)}) {
		got[r.Name] = r
	}
	if r := got[CheckGH]; r.Status != OK || !strings.Contains(r.Detail, "GitLab (gitlab.com) via glab") {
		t.Fatalf("git provider = %+v", r)
	}
	for _, name := range []string{CheckMergeSettings, CheckProtection, CheckGhStack} {
		if r := got[name]; r.Status != OK || !strings.Contains(r.Detail, "skipped on GitLab") {
			t.Errorf("%s = %+v, want skipped on GitLab", name, r)
		}
	}
}
