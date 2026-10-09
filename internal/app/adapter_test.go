package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

func TestSpawnWithCodexAdapter(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	if _, err := a.Spawn(SpawnReq{Title: "bad", Adapter: "nope"}); err == nil || !strings.Contains(err.Error(), "unknown adapter") {
		t.Fatalf("unknown adapter: err = %v", err)
	}
	if ts, _ := a.Store.Tasks(); len(ts) != 0 {
		t.Fatalf("unknown adapter left tasks behind: %+v", ts)
	}
	c, err := a.Spawn(SpawnReq{Title: "codex work", Adapter: "codex"})
	must(t, err)
	run := a.stateDir("run", c.ID)
	if got := agent.Recorded(run); got != "codex" {
		t.Errorf("recorded adapter %q", got)
	}
	script, err := os.ReadFile(filepath.Join(run, "launch.sh"))
	must(t, err)
	if !strings.Contains(string(script), "'codex'") || strings.Contains(string(script), "'opus'") {
		t.Errorf("codex launch should not inherit the claude model:\n%s", script)
	}
	if c.Model != "" {
		t.Errorf("codex task model = %q, want the CLI's default", c.Model)
	}
	cl, err := a.Spawn(SpawnReq{Title: "claude work"})
	must(t, err)
	if agent.Recorded(a.stateDir("run", cl.ID)) != "claude" || cl.Model != a.Cfg.Claude.Model {
		t.Errorf("default spawn is not claude: %+v", cl)
	}
}

// A hookless agent never calls the hook that hands out notices, and its
// status never turns idle, so notices are typed in whole.
func TestNotifyTypesNoticesIntoHooklessAgent(t *testing.T) {
	t.Parallel()
	a, ft := setup(t)
	c, err := a.Spawn(SpawnReq{Title: "codex work", Adapter: "codex"})
	must(t, err)
	must(t, a.Notify(c.ID, store.NoticeInfo, "t3 landed"))
	if len(ft.sent[c.Window]) != 0 {
		t.Fatalf("info notice typed: %q", ft.sent[c.Window])
	}
	must(t, a.Notify(c.ID, store.NoticeAction, "rebase onto integration"))
	sent := ft.sentTo(c.Window)
	if len(sent) != 1 || !strings.Contains(sent[0], "rebase onto integration") || !strings.Contains(sent[0], "t3 landed") {
		t.Fatalf("sent = %q", sent)
	}
	if !waitUntil(func() bool { n, _ := a.Store.PendingNotices(c.ID); return n == 0 }) {
		t.Errorf("notices still pending after the pane showed them")
	}
}

// #183: a notice typed into a hookless agent's pane is marked delivered only
// once the pane changed, and is typed with Enter exactly once.
func TestHooklessNoticeStaysPendingUntilPaneChanges(t *testing.T) {
	t.Parallel()
	a, ft := setup(t)
	fastRetry(t)
	ft.frozen = true
	c, err := a.Spawn(SpawnReq{Title: "codex work", Adapter: "codex"})
	must(t, err)
	must(t, a.Notify(c.ID, store.NoticeAction, "rebase onto integration"))
	time.Sleep(300 * time.Millisecond)
	if n, _ := a.Store.PendingNotices(c.ID); n != 1 {
		t.Fatalf("%d pending; the pane never changed, so the notice must stay pending", n)
	}
	if s := ft.sentTo(c.Window); len(s) != 1 {
		t.Fatalf("typed %d times, want exactly 1: %q", len(s), s)
	}
}

func waitUntil(cond func() bool) bool {
	for range 200 {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// Claims are advisory for hookless agents: done goes through, and the
// orchestrator hears about files that belong to another task.
func TestDoneFlagsHooklessWritesToClaimedFiles(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	owner, err := a.Spawn(SpawnReq{Title: "meter", Claims: []string{"billing/meter.go"}})
	must(t, err)
	c, err := a.Spawn(SpawnReq{Title: "codex work", Adapter: "codex"})
	must(t, err)
	write(t, c.Worktree, "billing/meter.go", "package billing\n\nfunc Meter() int { return 2 }\n")
	write(t, c.Worktree, "docs/new.md", "new\n")
	commitAll(t, c.Worktree, "codex edits")
	_, _ = a.Store.TakeNotices(OrchestratorID, false)
	must(t, a.Done(c.ID, "did it"))
	ns, err := a.Store.TakeNotices(OrchestratorID, false)
	must(t, err)
	text := store.FormatNotices(ns)
	if !strings.Contains(text, "billing/meter.go") || !strings.Contains(text, owner.ID) || strings.Contains(text, "docs/new.md") {
		t.Errorf("orchestrator notices:\n%s", text)
	}
}

func TestUsageMeterReadsCodexRollout(t *testing.T) {
	t.Parallel()
	a, m := meterApp(t)
	wt := filepath.Join(a.Root, ".saddle", "worktrees", "t5-codex")
	must(t, a.Store.CreateTask(store.Task{ID: "t5", Title: "codex", Role: store.RoleWorker, Worktree: wt, Status: store.Running}))
	write(t, a.stateDir("run", "t5"), "adapter", "codex\n")
	m.CodexHome = filepath.Join(a.Root, "codex")
	b, err := os.ReadFile(filepath.Join("..", "usage", "testdata", "codex-rollout.jsonl"))
	must(t, err)
	write(t, m.CodexHome, "sessions/2026/10/02/rollout-1.jsonl", strings.ReplaceAll(string(b), `"/repo/wt"`, `"`+wt+`"`))
	must(t, m.Sync())
	must(t, m.Sync())
	if got := modelTotals(t, a)[usage.CodexModel]; got != (usage.Tokens{Input: 800, CacheRead: 2900, Output: 130}) {
		t.Errorf("codex usage = %+v", got)
	}
	u, err := a.Usage(time.Now())
	must(t, err)
	if len(u.Tasks) != 1 || u.Tasks[0].Key != "t5" {
		t.Errorf("task totals %+v", u.Tasks)
	}
}

// #142: [adapters.<name>] sets the command and arguments of non-Claude agents.
func TestAdapterCmdFromConfig(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	if cmd, args := a.adapterCmd("codex"); cmd != "" || args != nil {
		t.Fatalf("codex without config = %q %v, want the default binary", cmd, args)
	}
	a.Cfg.Adapters = map[string]config.Adapter{"codex": {Cmd: "/opt/codex", Args: []string{"--full-auto"}}}
	if cmd, args := a.adapterCmd("codex"); cmd != "/opt/codex" || len(args) != 1 || args[0] != "--full-auto" {
		t.Fatalf("codex = %q %v", cmd, args)
	}
	if cmd, _ := a.adapterCmd(usage.Claude); cmd != a.Cfg.Claude.Cmd {
		t.Fatalf("claude = %q", cmd)
	}
	c, err := a.Spawn(SpawnReq{Title: "codex work", Adapter: "codex"})
	must(t, err)
	script, err := os.ReadFile(filepath.Join(a.stateDir("run", c.ID), "launch.sh"))
	must(t, err)
	if !strings.Contains(string(script), "/opt/codex") || !strings.Contains(string(script), "--full-auto") {
		t.Errorf("launch ignores adapters.codex:\n%s", script)
	}
}

// harness = "grok" makes grok the default worker with hooks enforced, and
// still honors [adapters.grok] cmd and args from #142.
func TestSpawnUnderGrokHarness(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	a.Cfg.Harness = config.HarnessGrok
	a.Cfg.Grok = config.Grok{Cmd: "grok", Model: "grok-4.5", PermissionMode: "bypassPermissions"}
	a.Cfg.Adapters = map[string]config.Adapter{"grok": {Cmd: "/opt/grok", Args: []string{"--sandbox"}}}
	g, err := a.Spawn(SpawnReq{Title: "grok work"})
	must(t, err)
	run := a.stateDir("run", g.ID)
	if agent.Recorded(run) != usage.Grok || g.Model != "grok-4.5" {
		t.Fatalf("default spawn under grok harness: adapter %q model %q", agent.Recorded(run), g.Model)
	}
	if !a.taskAdapter(g).Hooks() {
		t.Error("grok harness worker should have hooks, so claims are enforced")
	}
	script, err := os.ReadFile(filepath.Join(run, "launch.sh"))
	must(t, err)
	for _, want := range []string{"'/opt/grok'", "'--sandbox'", "'--trust'", "'bypassPermissions'"} {
		if !strings.Contains(string(script), want) {
			t.Errorf("launch.sh missing %s:\n%s", want, script)
		}
	}
	c, err := a.Spawn(SpawnReq{Title: "claude work", Adapter: usage.Claude})
	must(t, err)
	if agent.Recorded(a.stateDir("run", c.ID)) != usage.Claude || c.Model != a.Cfg.Claude.Model {
		t.Errorf("explicit claude under grok harness: %+v", c)
	}
}
