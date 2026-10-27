package doctor

import (
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent/agenttest"
)

// A VM with only codex, logged in somewhere other than ~/.codex/auth.json
// (its keyring store, say): the orchestrator and workers run codex, and
// doctor doesn't fail for want of Claude.
func TestCodexInstalledWithoutAuthFileRunsCodex(t *testing.T) {
	agenttest.Path(t, []string{"gh", "codex"}, []string{"gh"})
	home := t.TempDir()
	codexHome = func() string { return home }
	t.Cleanup(func() { codexHome = defaultCodexHome })
	f := healthy(t)
	rs := Run(pathEnv{f, System(f.root)})
	if r := find(t, rs, CheckClaude); r.Status == Fail {
		t.Errorf("claude: %s %q, want it not used", r.Status, r.Detail)
	}
	if r := find(t, rs, CheckOrchestrator); r.Status == Fail || !strings.Contains(r.Detail, "runs on codex") {
		t.Errorf("orchestrator: %s %q, want it on codex", r.Status, r.Detail)
	}
	if r := find(t, rs, CheckAgents); r.Status != Warn || !strings.Contains(r.Detail, "codex login") {
		t.Errorf("agent CLIs: %s %q, want a warning that codex may be logged out", r.Status, r.Detail)
	}
}
