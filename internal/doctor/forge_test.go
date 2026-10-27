package doctor

import (
	"errors"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent/agenttest"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/forge"
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
		// A GitLab-only VM whose remote still points at github.com: say glab is
		// there, and that the remote (or [forge]) is what to fix.
		{"github, gh missing, glab installed", gh, []string{"glab"}, []string{"glab"}, config.Forge{}, "github", "", "glab is installed, but the remote is on github.com", "git remote set-url"},
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

const gitlabProject = `{"path_with_namespace":"g/r","web_url":"https://gitlab.com/g/r","merge_method":"ff","squash_option":"default_on",` +
	`"default_branch":"main","only_allow_merge_if_pipeline_succeeds":true}`

const gitlabProtected = `{"name":"main","push_access_levels":[{"access_level_description":"No one"}],` +
	`"merge_access_levels":[{"access_level_description":"Maintainers"}],"allow_force_push":false}`

// onGitLab is a healthy fake repo on gitlab.com whose glab (a fake binary
// on PATH) is logged in, answering glab api from f.glab.
func onGitLab(t *testing.T) *fakeEnv {
	agenttest.Path(t, []string{"glab", "claude"}, []string{"glab", "claude"})
	f := healthy(t)
	f.git["remote get-url origin"] = res{out: "git@gitlab.com:g/r.git"}
	f.glab = map[string]res{
		"api projects/:id":                         {out: gitlabProject},
		"api projects/:id/protected_branches/main": {out: gitlabProtected},
	}
	return f
}

// #648: on GitLab through glab doctor reads the merge settings and the
// protected branch through the forge, never calls gh, and doesn't offer
// gh-stack.
func TestDoctorOnGitLab(t *testing.T) {
	f := onGitLab(t)
	got := map[string]Result{}
	for _, r := range Run(pathEnv{f, System(f.root)}) {
		got[r.Name] = r
	}
	if r := got[CheckGH]; r.Status != OK || !strings.Contains(r.Detail, "GitLab (gitlab.com) via glab") {
		t.Fatalf("git provider = %+v", r)
	}
	for name, want := range map[string]string{
		CheckMergeSettings: "fast-forward merge, squash allowed",
		CheckProtection:    "main: protected (push: No one; merge: Maintainers), pipelines must succeed",
		CheckGhStack:       "gh-stack is GitHub-only and this repo is on GitLab",
	} {
		if r := got[name]; r.Status != OK || !strings.Contains(r.Detail, want) {
			t.Errorf("%s = %+v, want ok with %q", name, r, want)
		}
	}
	if len(f.ghCalls) > 0 {
		t.Errorf("doctor called gh on a GitLab repo: %q", f.ghCalls)
	}
}

// #648: what doctor says about a GitLab project's settings when they need
// work, and when the forge can't tell.
func TestDoctorOnGitLabProblems(t *testing.T) {
	for _, tc := range []struct {
		name   string
		check  string
		break_ func(f *fakeEnv)
		status Status
		want   string // substring of Detail + " / " + Fix
	}{
		{"merge commits", CheckMergeSettings, func(f *fakeEnv) {
			f.glab["api projects/:id"] = res{out: strings.Replace(gitlabProject, `"ff"`, `"merge"`, 1)}
		}, Fail, `https://gitlab.com/g/r/-/settings/merge_requests set Merge method to "Fast-forward merge"`},
		{"squash never", CheckMergeSettings, func(f *fakeEnv) {
			f.glab["api projects/:id"] = res{out: strings.Replace(gitlabProject, `"default_on"`, `"never"`, 1)}
		}, Warn, "Squash commits when merging"},
		{"merge settings unreadable", CheckMergeSettings, func(f *fakeEnv) {
			f.glab["api projects/:id"] = res{err: errors.New("glab api: 403 Forbidden")}
		}, Warn, "Maintainer access"},
		{"unprotected", CheckProtection, func(f *fakeEnv) {
			f.glab["api projects/:id/protected_branches/main"] = res{err: errors.New("glab api: 404 Not Found")}
		}, Warn, "https://gitlab.com/g/r/-/settings/repository and protect main"},
		{"pipelines needn't succeed", CheckProtection, func(f *fakeEnv) {
			f.glab["api projects/:id"] = res{out: strings.Replace(gitlabProject, `"only_allow_merge_if_pipeline_succeeds":true`, `"only_allow_merge_if_pipeline_succeeds":false`, 1)}
		}, Warn, `tick "Pipelines must succeed"`},
		{"protection unreadable", CheckProtection, func(f *fakeEnv) {
			f.glab["api projects/:id/protected_branches/main"] = res{err: errors.New("glab api: 403 Forbidden")}
		}, Warn, "Protected branches by hand"},
		{"gh-stack wanted", CheckGhStack, func(f *fakeEnv) { f.cfg.Train.StackBackend = config.StackBackendGhStack }, OK, "saddle chains PR and MR targets itself"},
		{"default branch from glab", CheckDefaultBranch, func(f *fakeEnv) {
			delete(f.git, "symbolic-ref --short refs/remotes/origin/HEAD")
		}, OK, "main (matches base)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := onGitLab(t)
			tc.break_(f)
			r := find(t, Run(pathEnv{f, System(f.root)}), tc.check)
			text := r.Detail + " / " + r.Fix
			if r.Status != tc.status || !strings.Contains(text, tc.want) {
				t.Fatalf("%s = %s %q, want %s with %q", tc.check, r.Status, text, tc.status, tc.want)
			}
			if len(f.ghCalls) > 0 {
				t.Errorf("doctor called gh on a GitLab repo: %q", f.ghCalls)
			}
		})
	}
}

// #648: a forge that doesn't report branch protection degrades to a
// warning, not a failure.
func TestProtectionUnsupportedWarns(t *testing.T) {
	r := &run{env: healthy(t), base: "main", ready: true, provider: config.ForgeGitHub, fg: forge.Unsupported{Name: "github", Reason: "test"}}
	res := r.protection()
	if res.Status != Warn || !strings.Contains(res.Detail, "doesn't report it") || !strings.Contains(res.Fix, "by hand") {
		t.Fatalf("protection on an unsupporting forge = %+v", res)
	}
}
