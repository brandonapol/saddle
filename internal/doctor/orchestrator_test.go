package doctor

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent/agenttest"
	"github.com/brandonapol/saddle/internal/config"
)

// orchRows are the rows that say what the orchestrator runs on and whether
// it can.
var orchRows = []string{CheckClaude, CheckAgents, CheckOrchestrator, CheckOrchAllow, CheckGrok}

// #668: saddle doctor's orchestrator rows with claude only, codex only (before
// and after saddle init writes it) and both.
func TestOrchestratorGoldens(t *testing.T) {
	for _, tc := range []struct {
		name    string
		bins    []string
		harness string
		adapter string
		model   string
	}{
		{name: "claude-only", bins: []string{"gh", "claude"}},
		{name: "codex-only", bins: []string{"gh", "codex"}},
		{name: "codex-only-initialized", bins: []string{"gh", "codex"}, harness: "codex", adapter: "codex"},
		{name: "both", bins: []string{"gh", "claude", "codex"}},
		{name: "both-codex-orchestrator", bins: []string{"gh", "claude", "codex"}, adapter: "codex", model: "gpt-5-codex"},
		{name: "grok-orchestrator", bins: []string{"gh", "claude", "grok"}, adapter: "grok"},
		{name: "codex-configured-but-missing", bins: []string{"gh", "claude"}, adapter: "codex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agenttest.Path(t, tc.bins, tc.bins)
			home := t.TempDir()
			codexHome = func() string { return home }
			t.Cleanup(func() { codexHome = defaultCodexHome })
			f := healthy(t)
			if tc.harness != "" {
				f.cfg.Harness = tc.harness
			}
			f.cfg.Orchestrator.Adapter, f.cfg.Orchestrator.Model = tc.adapter, tc.model
			rs := Run(pathEnv{f, System(f.root)})
			var keep []Result
			for _, r := range rs {
				if slices.Contains(orchRows, r.Name) {
					keep = append(keep, r)
				}
			}
			var out bytes.Buffer
			WriteTable(&out, keep)
			golden(t, filepath.Join("testdata", "orchestrator", tc.name+".golden"), out.Bytes())
		})
	}
}

func TestCodexOrchestratorCheck(t *testing.T) {
	home := t.TempDir()
	codexHome = func() string { return home }
	t.Cleanup(func() { codexHome = defaultCodexHome })
	base := func() *fakeEnv {
		f := healthy(t)
		f.cfg.Orchestrator.Adapter = "codex"
		f.paths["codex"] = true
		f.exec["codex --version"] = res{out: "codex-cli 0.50.0"}
		f.exec["codex exec --help"] = res{out: "Usage: codex exec\n      --json  Print events as JSONL"}
		return f
	}
	if r := find(t, Run(base()), CheckOrchestrator); r.Status != OK || !strings.Contains(r.Detail, "codex-cli 0.50.0") ||
		!strings.Contains(r.Detail, "set by [orchestrator] adapter") || !strings.Contains(r.Detail, "between turns") {
		t.Errorf("healthy codex: %s %q", r.Status, r.Detail)
	}
	for name, tc := range map[string]struct {
		mod    func(f *fakeEnv)
		status Status
		want   string
	}{
		"old codex": {func(f *fakeEnv) { f.exec["codex exec --help"] = res{out: "Usage: codex exec"} }, Fail, "no --json"},
		"broken":    {func(f *fakeEnv) { delete(f.exec, "codex --version") }, Fail, "--version` fails"},
		"read-only": {func(f *fakeEnv) { f.cfg.Claude.PermissionMode = "plan" }, Warn, "read-only sandbox"},
		"no network": {func(f *fakeEnv) {
			f.cfg.Adapters = config.AdapterMap{"codex": {Args: []string{"-c", `sandbox_mode="workspace-write"`}}}
		}, Warn, "without network"},
		"network on": {func(f *fakeEnv) {
			f.cfg.Adapters = config.AdapterMap{"codex": {Args: []string{"-c", `sandbox_mode="workspace-write"`,
				"-c", "sandbox_workspace_write.network_access=true"}}}
		}, OK, "with network"},
		"custom cmd": {func(f *fakeEnv) {
			f.cfg.Adapters = config.AdapterMap{"codex": {Cmd: "/opt/codex"}}
			f.exec["/opt/codex --version"] = res{out: "codex-cli 9"}
			f.exec["/opt/codex exec --help"] = res{out: "--json"}
		}, OK, "codex-cli 9"},
	} {
		f := base()
		tc.mod(f)
		r := find(t, Run(f), CheckOrchestrator)
		if r.Status != tc.status || !strings.Contains(r.Detail, tc.want) || r.Status != OK && r.Fix == "" {
			t.Errorf("%s: %s %q (fix %q), want %s with %q", name, r.Status, r.Detail, r.Fix, tc.status, tc.want)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[mcp_servers.saddle]\nurl = \"http://localhost:7\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := find(t, Run(base()), CheckOrchestrator); r.Status != Fail || !strings.Contains(r.Detail, "with a url") ||
		!strings.Contains(r.Fix, "remove the [mcp_servers.saddle] table") {
		t.Errorf("saddle MCP over url: %s %q (fix %q)", r.Status, r.Detail, r.Fix)
	}
	// A non-Claude orchestrator needs no Claude allowlist.
	f := base()
	f.allow = nil
	for _, r := range Run(f) {
		if r.Name == CheckOrchAllow {
			t.Errorf("orchestrator allowlist checked for a codex orchestrator: %+v", r)
		}
	}
}
