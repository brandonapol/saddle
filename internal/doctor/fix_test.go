package doctor

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/refguard"
)

// halfInit is the repo from #163: something created .saddle/state.db, but
// saddle init never ran, so there is no config.toml, no hooks and no exclude.
func halfInit(t *testing.T) *fakeEnv {
	f := healthy(t)
	delete(f.paths, filepath.Join(f.root, ".saddle", "config.toml"))
	f.paths[filepath.Join(f.root, ".saddle", "state.db")] = true
	f.hooks = []refguard.HookState{{Name: "reference-transaction"}, {Name: "pre-push"}}
	delete(f.git, "check-ignore -q .saddle/")
	return f
}

// initFake does what saddle init does to the fake env.
func initFake(f *fakeEnv, calls *int) func() error {
	return func() error {
		*calls++
		f.paths[filepath.Join(f.root, ".saddle", "config.toml")] = true
		f.hooks = saddleHooks()
		f.git["check-ignore -q .saddle/"] = res{}
		return nil
	}
}

func TestConfigMissingIsNotOK(t *testing.T) {
	f := healthy(t)
	delete(f.paths, filepath.Join(f.root, ".saddle", "config.toml"))
	c := find(t, Run(f), CheckConfig)
	if c.Status != Warn || !c.Fixable || !strings.Contains(c.Detail, "defaults (no .saddle/config.toml)") || !strings.Contains(c.Fix, "saddle doctor --fix") {
		t.Fatalf("config without config.toml = %+v", c)
	}

	c = find(t, Run(halfInit(t)), CheckConfig)
	if !strings.Contains(c.Detail, "half set up") || !strings.Contains(c.Detail, "saddle init never ran") {
		t.Fatalf("half-initialized config = %+v", c)
	}
}

func TestLocalFailuresAreFixable(t *testing.T) {
	rs := Run(halfInit(t))
	for _, name := range []string{CheckConfig, CheckHooks, CheckIgnored} {
		c := find(t, rs, name)
		if !c.Fixable || c.Group() != GroupFixable || !strings.Contains(c.Fix, "saddle init") {
			t.Errorf("%s = %+v (group %s), want fixable by saddle init", name, c, c.Group())
		}
	}
	f := healthy(t)
	f.paths["gh"] = false
	if c := find(t, Run(f), CheckGH); c.Fixable || c.Group() != GroupManual {
		t.Errorf("gh auth = %+v (group %s), want manual", c, c.Group())
	}
	f = healthy(t)
	f.leftovers = 2
	if c := find(t, Run(f), CheckLeftovers); c.Group() != GroupOptional {
		t.Errorf("leftovers group = %s, want optional", c.Group())
	}
}

func TestFixRunsInitOnceAndMarksFixed(t *testing.T) {
	f := halfInit(t)
	calls := 0
	rs, err := Fix(f, initFake(f, &calls))
	if err != nil || calls != 1 {
		t.Fatalf("Fix: err %v, init ran %d times", err, calls)
	}
	if Failed(rs) {
		t.Fatalf("still failing after Fix: %+v", rs)
	}
	for _, name := range []string{CheckConfig, CheckHooks, CheckIgnored} {
		if c := find(t, rs, name); c.Status != OK || !c.Fixed || c.Group() != GroupFixed {
			t.Errorf("%s = %+v, want ok and fixed", name, c)
		}
	}
	if c := find(t, rs, CheckTmux); c.Fixed {
		t.Errorf("tmux marked fixed: %+v", c)
	}

	// Nothing to fix: init doesn't run.
	calls = 0
	if _, err := Fix(healthy(t), initFake(f, &calls)); err != nil || calls != 0 {
		t.Fatalf("Fix on a healthy repo: err %v, init ran %d times", err, calls)
	}

	// init's error comes back with the checks as they were.
	f = halfInit(t)
	rs, err = Fix(f, func() error { return errors.New("boom") })
	if err == nil || !Failed(rs) {
		t.Fatalf("Fix with failing init: err %v failed %v", err, Failed(rs))
	}
}

func TestEveryCheckExplainsItself(t *testing.T) {
	for _, r := range Run(halfInit(t)) {
		if r.About == "" {
			t.Errorf("%s has no plain-words explanation", r.Name)
		}
	}
	if c := find(t, Run(healthy(t)), CheckHooks); !strings.Contains(c.About, "stop agents from moving saddle's own branches") {
		t.Errorf("hooks about = %q", c.About)
	}
}

func TestProtectionNamesURLStepsAndJobs(t *testing.T) {
	f := healthy(t)
	f.gh["api repos/{owner}/{repo}/branches/main/protection"] = res{err: errors.New("gh: Branch not protected (HTTP 404)")}
	write(t, filepath.Join(f.root, ".github", "workflows", "ci.yml"), `name: CI
on: [push]
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - run: make check
  secrets:
    name: secrets
    runs-on: ubuntu-latest
    steps:
      - run: gitleaks
`)
	write(t, filepath.Join(f.root, ".github", "workflows", "release.yaml"), "on: tag\njobs:\n  publish:\n    name: Publish release\n    runs-on: x\n")
	c := find(t, Run(f), CheckProtection)
	for _, want := range []string{
		"https://github.com/o/r/settings/branches", "Require a pull request before merging",
		"Require status checks to pass", "check, secrets, Publish release", "after it has run once",
	} {
		if !strings.Contains(c.Fix, want) {
			t.Errorf("protection fix lacks %q: %s", want, c.Fix)
		}
	}
	if c.Group() != GroupOptional {
		t.Errorf("protection group = %s", c.Group())
	}

	// No workflows: say so instead of naming jobs.
	f = healthy(t)
	f.gh["api repos/{owner}/{repo}/branches/main/protection"] = res{err: errors.New("HTTP 404")}
	if c := find(t, Run(f), CheckProtection); !strings.Contains(c.Fix, "no workflows in .github/workflows") {
		t.Errorf("protection fix without workflows: %s", c.Fix)
	}
}

func TestRepoSlugFromRemote(t *testing.T) {
	for url, want := range map[string]string{
		"git@github.com:o/r.git":          "o/r",
		"https://github.com/o/r":          "o/r",
		"https://github.com/o/r.git":      "o/r",
		"ssh://git@github.com/o/r.git":    "o/r",
		"/tmp/origin.git":                 "",
		"https://gitlab.com/o/r.git":      "",
		"https://user@github.com/o/r.git": "o/r",
	} {
		if got := repoSlug(url); got != want {
			t.Errorf("repoSlug(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestWriteTableGroupsAndNextSteps(t *testing.T) {
	f := halfInit(t)
	f.paths["gh"] = false
	var out bytes.Buffer
	WriteTable(&out, Run(f))
	s := out.String()
	fixable := strings.Index(s, "Saddle can fix these")
	manual := strings.Index(s, "You need to do this")
	if fixable < 0 || manual < fixable || !strings.Contains(s, "saddle doctor --fix") {
		t.Fatalf("grouped table:\n%s", s)
	}
	if strings.Contains(s, "Next steps") {
		t.Errorf("next steps shown while blocked:\n%s", s)
	}
	if !strings.Contains(s[manual:], CheckGH) || strings.Contains(s[fixable:manual], CheckGH) {
		t.Errorf("gh auth not under manual:\n%s", s)
	}

	f = halfInit(t)
	calls := 0
	rs, err := Fix(f, initFake(f, &calls))
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	WriteTable(&out, rs)
	s = out.String()
	for _, want := range []string{"Fixed automatically", CheckHooks, "Next steps", "saddle up"} {
		if !strings.Contains(s, want) {
			t.Errorf("table after fix lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "Saddle can fix these") {
		t.Errorf("fixable section after fix:\n%s", s)
	}
}

func TestWriteJSONIncludesGroups(t *testing.T) {
	var js bytes.Buffer
	if err := WriteJSON(&js, Run(halfInit(t))); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Checks []struct {
			Name    string `json:"name"`
			Group   string `json:"group"`
			About   string `json:"about"`
			Fixable bool   `json:"fixable"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(js.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	groups := map[string]string{}
	for _, c := range got.Checks {
		groups[c.Name] = c.Group
		if c.About == "" {
			t.Errorf("%s: no about in JSON", c.Name)
		}
	}
	if groups[CheckHooks] != GroupFixable || groups[CheckTmux] != GroupOK {
		t.Fatalf("groups %v\n%s", groups, js.String())
	}
}
