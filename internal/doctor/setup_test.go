package doctor

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/agent/agenttest"
	"github.com/brandonapol/saddle/internal/config"
)

var update = flag.Bool("update", false, "rewrite the doctor goldens in testdata/setup")

// pathEnv is a healthy fake repo whose provider and agent CLIs are real
// programs on PATH, so agenttest's fake binaries decide them.
type pathEnv struct {
	*fakeEnv
	sys Env
}

var realCLIs = []string{"gh", "glab", "claude", "codex", "grok"}

func (p pathEnv) LookPath(name string) (string, error) {
	if slices.Contains(realCLIs, name) {
		return p.sys.LookPath(name)
	}
	return p.fakeEnv.LookPath(name)
}

func (p pathEnv) Exec(name string, args ...string) (string, error) {
	if slices.Contains(realCLIs, name) {
		return p.sys.Exec(name, args...)
	}
	return p.fakeEnv.Exec(name, args...)
}

func (p pathEnv) Agents(cfg config.Config) []agent.Status {
	return p.sys.(AgentsEnv).Agents(cfg)
}

// #646: saddle doctor's git provider and agent CLI rows for each mix of gh,
// glab, claude and codex on PATH.
func TestSetupGoldens(t *testing.T) {
	const github, gitlab = "git@github.com:o/r.git", "git@gitlab.com:g/r.git"
	for _, tc := range []struct {
		name   string
		remote string
		bins   []string
		logins []string // nil: every bin is logged in
	}{
		{"gh-only", github, []string{"gh", "claude"}, nil},
		{"glab-only", gitlab, []string{"glab", "claude"}, nil},
		{"both", github, []string{"gh", "glab", "claude"}, nil},
		{"both-on-gitlab", gitlab, []string{"gh", "glab", "claude"}, nil},
		{"neither", github, []string{"claude"}, nil},
		{"claude-missing-codex-present", github, []string{"gh", "codex"}, nil},
		{"nothing", github, nil, nil},
		{"claude-logged-out-codex-present", github, []string{"gh", "claude", "codex"}, []string{"gh", "codex"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logins := tc.logins
			if logins == nil {
				logins = tc.bins
			}
			agenttest.Path(t, tc.bins, logins)
			f := healthy(t)
			f.git["remote get-url origin"] = res{out: tc.remote}
			rs := Run(pathEnv{f, System(f.root)})
			var keep []Result
			for _, r := range rs {
				if r.Name == CheckGH || r.Name == CheckClaude || r.Name == CheckAgents {
					keep = append(keep, r)
				}
			}
			var out bytes.Buffer
			WriteTable(&out, keep)
			golden(t, filepath.Join("testdata", "setup", tc.name+".golden"), out.Bytes())
		})
	}
}

func golden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -run %s -update)", err, t.Name())
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs:\n--- got\n%s\n--- want\n%s", path, got, want)
	}
}
