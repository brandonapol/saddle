package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
)

func launchSh(t *testing.T, a *App, task string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(a.stateDir("run", task), "launch.sh"))
	must(t, err)
	return string(b)
}

// #150: the session harness picks the default worker; an explicit adapter
// still wins (#182).
func TestSpawnDefaultFollowsHarness(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.Cfg.Harness = config.HarnessCodex
	c, err := a.Spawn(SpawnReq{Title: "default"})
	must(t, err)
	if got := agent.Recorded(a.stateDir("run", c.ID)); got != "codex" || c.Model != "" {
		t.Errorf("harness codex: spawned %s with model %q", got, c.Model)
	}
	cl, err := a.Spawn(SpawnReq{Title: "explicit", Adapter: "claude"})
	must(t, err)
	if got := agent.Recorded(a.stateDir("run", cl.ID)); got != "claude" || cl.Model != a.Cfg.Claude.Model {
		t.Errorf("explicit claude: spawned %s with model %q", got, cl.Model)
	}
	g, err := a.Spawn(SpawnReq{Title: "gemini", Adapter: "gemini"})
	must(t, err)
	if got := agent.Recorded(a.stateDir("run", g.ID)); got != "gemini" {
		t.Errorf("explicit gemini: spawned %s", got)
	}
}

// #181: a grok spawn from a claude session runs the grok harness with the
// [grok] settings, not the one-shot launch grok rejects.
func TestSpawnGrokFromClaudeSessionRunsHarness(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.Cfg.Grok.Model = "grok-4.5"
	g, err := a.Spawn(SpawnReq{Title: "grok work", Adapter: "grok"})
	must(t, err)
	if g.Model != "grok-4.5" {
		t.Errorf("grok model = %q", g.Model)
	}
	sh := launchSh(t, a, g.ID)
	for _, want := range []string{"'--permission-mode' '" + a.Cfg.Grok.PermissionMode + "'", "'--model' 'grok-4.5'"} {
		if !strings.Contains(sh, want) {
			t.Errorf("launch.sh lacks %s:\n%s", want, sh)
		}
	}
	if !a.taskAdapter(g).Hooks() {
		t.Error("a grok worker has hooks")
	}
}

// #182: spawning an adapter that can't run fails with the reason and the
// adapters that can, and leaves no task behind. Force skips the check.
func TestSpawnUnavailableAdapter(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.AdapterStatus = func() []agent.Status {
		return []agent.Status{{Name: "claude", OK: true}, {Name: "codex", OK: true},
			{Name: "gemini", Reason: "gemini is not logged in"}, {Name: "grok", OK: true}}
	}
	_, err := a.Spawn(SpawnReq{Title: "g", Adapter: "gemini"})
	if err == nil || !strings.Contains(err.Error(), "not logged in") || !strings.Contains(err.Error(), "claude, codex, grok") {
		t.Fatalf("err = %v", err)
	}
	if ts, _ := a.Store.Tasks(); len(ts) != 0 {
		t.Fatalf("unavailable adapter left tasks: %+v", ts)
	}
	if _, err := a.Spawn(SpawnReq{Title: "g", Adapter: "gemini", Force: true}); err != nil {
		t.Fatalf("force: %v", err)
	}
}

// #150: a recorded session of another agent is not resumed.
func TestOrchestratorResumeOnlySameAgent(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	o, err := a.EnsureOrchestrator()
	must(t, err)
	must(t, a.Store.SetField(o.ID, "session_id", "sess-1"))
	run := a.stateDir("run", o.ID)
	must(t, os.MkdirAll(run, 0o755))
	must(t, os.WriteFile(filepath.Join(run, "adapter"), []byte("claude\n"), 0o644))
	if _, resume, err := a.Orchestrator(); err != nil || resume != "sess-1" {
		t.Fatalf("same agent: resume %q, %v", resume, err)
	}
	a.Cfg.Harness = config.HarnessGrok
	if _, resume, err := a.Orchestrator(); err != nil || resume != "" {
		t.Fatalf("claude session resumed into grok: %q, %v", resume, err)
	}
	if o, _ := a.Store.Task(o.ID); o.SessionID != "sess-1" {
		t.Errorf("the other agent's session id was dropped: %q", o.SessionID)
	}
}

// #182: the brief names the session's agent and lists the adapters a worker
// may run on.
func TestBriefListsAvailableAdapters(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.AdapterStatus = func() []agent.Status {
		return []agent.Status{{Name: "claude", OK: true}, {Name: "codex", OK: true}, {Name: "gemini", Reason: "gemini not on PATH"}}
	}
	b := a.PluginBrief()
	if !strings.Contains(b, "claude, codex") || !strings.Contains(b, "gemini (gemini not on PATH)") {
		t.Errorf("brief does not list adapters:\n%s", b)
	}
	if strings.Contains(b, "team of Claude Code agents") {
		t.Errorf("brief still says the team is Claude Code:\n%s", b)
	}
}
