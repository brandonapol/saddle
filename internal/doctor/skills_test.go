package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/orch"
)

// skillsEnv adds the orchestrator probe to the fake.
type skillsEnv struct {
	*fakeEnv
	cmds []orch.Command
	err  error
	disk map[string][]string
	runs *int
}

func (e skillsEnv) OrchestratorCommands(config.Config) ([]orch.Command, error) {
	if e.runs != nil {
		*e.runs++
	}
	return e.cmds, e.err
}
func (e skillsEnv) DiskSkills() map[string][]string { return e.disk }

func skillsResult(t *testing.T, env Env) (Result, bool) {
	t.Helper()
	for _, r := range Run(env) {
		if r.Name == CheckSkills {
			return r, true
		}
	}
	return Result{}, false
}

func TestSkillsCheck(t *testing.T) {
	disk := map[string][]string{"/home/me/.claude/skills": {"budget"}, "/r/.claude/skills": {"deploy"}}
	cases := []struct {
		name   string
		env    skillsEnv
		status Status
		detail []string
	}{
		{"lists them", skillsEnv{cmds: []orch.Command{{Name: "budget"}, {Name: "deploy"}, {Name: "loop"}}, disk: disk},
			OK, []string{"3 slash commands", "/budget", "/deploy", "/loop"}},
		{"empty while skills exist", skillsEnv{disk: disk},
			Warn, []string{"sees no skills or slash commands", "/home/me/.claude/skills", "/r/.claude/skills"}},
		{"empty and none on disk", skillsEnv{}, OK, []string{"no skills or slash commands"}},
		{"one on disk is missing", skillsEnv{cmds: []orch.Command{{Name: "budget"}}, disk: disk},
			Warn, []string{"doesn't see deploy", "/r/.claude/skills"}},
		{"probe fails", skillsEnv{err: errors.New("Invalid API key\nmore"), disk: disk},
			Warn, []string{"could not list", "Invalid API key"}},
	}
	for _, c := range cases {
		c.env.fakeEnv = healthy(t)
		r, ok := skillsResult(t, c.env)
		if !ok {
			t.Fatalf("%s: no %s check", c.name, CheckSkills)
		}
		if r.Status != c.status {
			t.Errorf("%s: status %s, want %s (%s)", c.name, r.Status, c.status, r.Detail)
		}
		for _, want := range c.detail {
			if !strings.Contains(r.Detail, want) {
				t.Errorf("%s: detail %q is missing %q", c.name, r.Detail, want)
			}
		}
		if r.Status != OK && r.Fix == "" {
			t.Errorf("%s: no fix", c.name)
		}
	}
}

// The probe starts a Claude session, so envs without it (saddle up's quick
// preflight) skip the check, and so does the grok harness.
func TestSkillsCheckSkipped(t *testing.T) {
	if _, ok := skillsResult(t, healthy(t)); ok {
		t.Fatal("skills check ran on an env without the probe")
	}
	runs := 0
	env := skillsEnv{fakeEnv: healthy(t), runs: &runs}
	env.cfg.Harness = config.HarnessGrok
	if _, ok := skillsResult(t, env); ok || runs != 0 {
		t.Fatalf("skills check ran under grok (%d probes)", runs)
	}
}

// fakeClaude writes a stand-in claude that runs body.
func fakeClaude(t *testing.T, body string) config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Claude.Cmd = p
	return cfg
}

func TestSystemProbesOrchestratorCommands(t *testing.T) {
	cfg := fakeClaude(t, `read req
case "$*" in *"--input-format stream-json"*) ;; *) echo "bad args: $*" >&2; exit 2;; esac
echo '{"type":"control_response","response":{"subtype":"success","request_id":"saddle-init","response":{"commands":[{"name":"budget","description":"d"}]}}}'
cat >/dev/null`)
	cs, err := probing{system(t.TempDir())}.OrchestratorCommands(cfg)
	if err != nil || len(cs) != 1 || cs[0].Name != "budget" {
		t.Fatalf("OrchestratorCommands = %+v, %v", cs, err)
	}
}

func TestSystemProbeTimesOut(t *testing.T) {
	prev := probeTimeout
	probeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { probeTimeout = prev })
	cfg := fakeClaude(t, "exec sleep 60")
	start := time.Now()
	_, err := probing{system(t.TempDir())}.OrchestratorCommands(cfg)
	if err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("probe err = %v after %s", err, time.Since(start))
	}
}

func TestSystemDiskSkills(t *testing.T) {
	home, root := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	for _, d := range []string{filepath.Join(home, ".claude", "skills", "budget"), filepath.Join(root, ".claude", "skills", "deploy")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got := probing{system(root)}.DiskSkills()
	if len(got) != 2 || got[filepath.Join(home, ".claude", "skills")][0] != "budget" || got[filepath.Join(root, ".claude", "skills")][0] != "deploy" {
		t.Fatalf("DiskSkills = %v", got)
	}
}
