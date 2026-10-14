package engine

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/app"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/gitx"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// screenTmux shows each window the screen set for it.
type screenTmux struct {
	n       int
	screens map[string]string
	keys    map[string][]string
	texts   map[string][]string
}

func (f *screenTmux) HasSession() bool                          { return true }
func (f *screenTmux) NewSession(n, d, c string) (string, error) { return f.NewWindow(n, d, c) }
func (f *screenTmux) NewWindow(string, string, string) (string, error) {
	f.n++
	return fmt.Sprintf("@%d", f.n), nil
}
func (f *screenTmux) KillWindow(string) error { return nil }
func (f *screenTmux) Alive(string) bool       { return true }
func (f *screenTmux) SendText(w, s string) error {
	if f.texts == nil {
		f.texts = map[string][]string{}
	}
	f.texts[w] = append(f.texts[w], s)
	return nil
}
func (f *screenTmux) SendKeys(w string, k ...string) error {
	f.keys[w] = append(f.keys[w], k...)
	return nil
}
func (f *screenTmux) Capture(w string, _ int) (string, error) { return f.screens[w], nil }
func (f *screenTmux) KillSession() error                      { return nil }

func setup(t *testing.T) (*app.App, *screenTmux) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir()) // RootTrusted reads ~/.claude.json
	t.Setenv("SADDLE_ROOT", "")
	t.Setenv("SADDLE_TASK", "")
	t.Setenv("SADDLE_TRAIN", "")
	for _, k := range []string{"GIT_AUTHOR_NAME", "GIT_COMMITTER_NAME"} {
		t.Setenv(k, "t")
	}
	for _, k := range []string{"GIT_AUTHOR_EMAIL", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, "t@example.com")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"}, {"commit", "-qm", "init"}} {
		if _, err := gitx.Run(root, args...); err != nil {
			t.Fatal(err)
		}
	}
	a, err := app.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	ft := &screenTmux{screens: map[string]string{}, keys: map[string][]string{}}
	a.Tmux = ft
	if _, err := a.EnsureOrchestrator(); err != nil {
		t.Fatal(err)
	}
	return a, ft
}

func orchNotices(t *testing.T, a *app.App) string {
	t.Helper()
	ns, err := a.Store.TakeNotices(app.OrchestratorID, true)
	if err != nil {
		t.Fatal(err)
	}
	return store.FormatNotices(ns)
}

func TestEngineReportsWorkerThatStoppedWithoutDone(t *testing.T) {
	a, ft := setup(t)
	w, err := a.Spawn(app.SpawnReq{Title: "meter"})
	if err != nil {
		t.Fatal(err)
	}
	ft.screens[w.Window] = "working…"
	e := New(a)
	must(t, e.Tick())
	orchNotices(t, a) // spawn notices, if any
	must(t, a.Store.SetStatus(w.ID, store.Idle))
	ft.screens[w.Window] = "all done, I think"
	must(t, e.Tick())
	got := orchNotices(t, a)
	if !strings.Contains(got, w.ID+" (meter) stopped without calling done") || !strings.Contains(got, "all done, I think") {
		t.Fatalf("notices %q", got)
	}
	must(t, e.Tick())
	if got := orchNotices(t, a); got != "" {
		t.Fatalf("reported twice: %q", got)
	}
}

func TestEngineFirstTickDoesNotReplayOldStates(t *testing.T) {
	a, _ := setup(t)
	w, err := a.Spawn(app.SpawnReq{Title: "meter"})
	if err != nil {
		t.Fatal(err)
	}
	must(t, a.Store.SetStatus(w.ID, store.Idle))
	orchNotices(t, a)
	must(t, New(a).Tick())
	if got := orchNotices(t, a); got != "" {
		t.Fatalf("a restarted engine re-reported: %q", got)
	}
}

func TestEngineReportsPromptThatSits(t *testing.T) {
	a, ft := setup(t)
	w, err := a.Spawn(app.SpawnReq{Title: "meter"})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	e := New(a)
	e.now = func() time.Time { return clock }
	must(t, e.Tick())
	orchNotices(t, a)

	ft.screens[w.Window] = "Do you want to proceed?\n❯ 1. Yes\n  2. No"
	must(t, e.Tick()) // first sight of the screen
	if got := orchNotices(t, a); got != "" {
		t.Fatalf("reported before the prompt settled: %q", got)
	}
	clock = clock.Add(5 * time.Second)
	must(t, e.Tick())
	if st, _ := a.Store.Task(w.ID); st.Status != store.NeedsYou {
		t.Fatalf("status %q", st.Status)
	}
	got := orchNotices(t, a)
	if !strings.Contains(got, w.ID+" (meter) is waiting on a permission prompt") || !strings.Contains(got, "Do you want to proceed?") {
		t.Fatalf("notices %q", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// limitScreen is Claude Code parked on a session usage limit (#180).
const limitScreen = `You've hit your session limit · resets 12:10pm (America/New_York)

● Usage limit reached · continuing automatically at 12:10pm · esc to cancel
╭────────────╮
│ >          │
╰────────────╯
  ⚠ Usage limit reached · limit resets 12:10pm`

func TestEngineParksWorkerOnUsageLimit(t *testing.T) {
	a, ft := setup(t)
	w, err := a.Spawn(app.SpawnReq{Title: "meter"})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	e := New(a)
	e.now = func() time.Time { return clock }
	ft.screens[w.Window] = "working…"
	must(t, e.Tick())
	orchNotices(t, a)

	ft.screens[w.Window] = limitScreen
	must(t, e.Tick())
	clock = clock.Add(5 * time.Second)
	must(t, e.Tick())
	if st, _ := a.Store.Task(w.ID); st.Status != app.StatusPaused {
		t.Fatalf("status %q while the banner is up", st.Status)
	}
	got := orchNotices(t, a)
	if !strings.Contains(got, w.ID+" (meter) is parked on a Claude usage limit") || !strings.Contains(got, "12:10pm (America/New_York)") {
		t.Fatalf("notices %q", got)
	}
	for range 3 {
		clock = clock.Add(5 * time.Second)
		must(t, e.Tick())
	}
	if got := orchNotices(t, a); got != "" {
		t.Fatalf("nagged again: %q", got)
	}
	if k := ft.keys[w.Window]; len(k) > 0 {
		t.Fatalf("sent keys into a parked pane: %v", k)
	}

	ft.screens[w.Window] = "⏺ Picking up where I left off"
	must(t, e.Tick())
	if st, _ := a.Store.Task(w.ID); st.Status != store.Running {
		t.Fatalf("status %q after the banner cleared", st.Status)
	}
	clock = clock.Add(5 * time.Second)
	must(t, e.Tick())
	if got := orchNotices(t, a); got != "" {
		t.Fatalf("notified on resume: %q", got)
	}
}

// Claude Code's Stop hook marks the worker idle as it hits the limit; that
// is the park, not a worker that stopped without calling done.
func TestEngineUsageLimitIsNotStoppedWithoutDone(t *testing.T) {
	a, ft := setup(t)
	w, err := a.Spawn(app.SpawnReq{Title: "meter"})
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	e := New(a)
	e.now = func() time.Time { return clock }
	ft.screens[w.Window] = "working…"
	must(t, e.Tick())
	orchNotices(t, a)

	must(t, a.Store.SetStatus(w.ID, store.Idle))
	ft.screens[w.Window] = limitScreen
	must(t, e.Tick())
	if got := orchNotices(t, a); got != "" {
		t.Fatalf("reported the park as a stop: %q", got)
	}
	clock = clock.Add(5 * time.Second)
	must(t, e.Tick())
	got := orchNotices(t, a)
	if strings.Contains(got, "stopped without calling done") || !strings.Contains(got, "parked on a Claude usage limit") {
		t.Fatalf("notices %q", got)
	}
	// The hook fires again while the banner is up: back to paused, quietly.
	must(t, a.Store.SetStatus(w.ID, store.Idle))
	clock = clock.Add(5 * time.Second)
	must(t, e.Tick())
	if st, _ := a.Store.Task(w.ID); st.Status != app.StatusPaused {
		t.Fatalf("status %q", st.Status)
	}
	if got := orchNotices(t, a); got != "" {
		t.Fatalf("nagged again: %q", got)
	}
}

// A restarted engine finds the worker it parked still paused on the banner,
// keeps quiet about it and still puts it back to running after the reset.
func TestRestartedEngineResumesUsageParkedWorker(t *testing.T) {
	a, ft := setup(t)
	w, err := a.Spawn(app.SpawnReq{Title: "meter"})
	if err != nil {
		t.Fatal(err)
	}
	must(t, a.Store.SetStatus(w.ID, app.StatusPaused))
	ft.screens[w.Window] = limitScreen
	orchNotices(t, a)
	clock := time.Now()
	e := New(a)
	e.now = func() time.Time { return clock }
	must(t, e.Tick())
	clock = clock.Add(5 * time.Second)
	must(t, e.Tick())
	if got := orchNotices(t, a); got != "" {
		t.Fatalf("re-notified after a restart: %q", got)
	}
	ft.screens[w.Window] = "⏺ Back at it"
	must(t, e.Tick())
	if st, _ := a.Store.Task(w.ID); st.Status != store.Running {
		t.Fatalf("status %q after the banner cleared", st.Status)
	}
}

// A task saddle down paused, whose window is gone, is not the engine's to
// resume.
func TestEngineLeavesDownPausedTaskAlone(t *testing.T) {
	a, ft := setup(t)
	w, err := a.Spawn(app.SpawnReq{Title: "meter"})
	if err != nil {
		t.Fatal(err)
	}
	must(t, a.Store.SetStatus(w.ID, app.StatusPaused))
	ft.screens[w.Window] = "⏺ last words"
	e := New(a)
	for range 3 {
		must(t, e.Tick())
	}
	if st, _ := a.Store.Task(w.ID); st.Status != app.StatusPaused {
		t.Fatalf("status %q", st.Status)
	}
}

// #321: a worker parked on its adapter's usage limit marks that adapter out
// of quota, so the next spawn rotates to grok; the banner clearing restores it.
func TestEngineParkRotatesAdapter(t *testing.T) {
	a, ft := setup(t)
	a.Cfg.AdapterPolicy.Failover = config.FailoverPark // the running worker waits; handoff has its own test
	a.AdapterStatus = func() []agent.Status {
		var out []agent.Status
		for _, n := range agent.Names() {
			out = append(out, agent.Status{Name: n, OK: true})
		}
		return out
	}
	var told []string
	a.OwnerNotify = func(s string) { told = append(told, s) }
	w, err := a.Spawn(app.SpawnReq{Title: "meter"})
	must(t, err)
	clock := time.Now()
	e := New(a)
	e.now = func() time.Time { return clock }
	ft.screens[w.Window] = limitScreen
	must(t, e.Tick())
	clock = clock.Add(5 * time.Second)
	must(t, e.Tick())
	if _, out := a.ExhaustedAdapters(time.Now())[usage.Claude]; !out {
		t.Fatal("claude not marked out of quota")
	}
	if len(told) != 1 || !strings.Contains(told[0], "grok") {
		t.Fatalf("owner told %q", told)
	}
	next, err := a.Spawn(app.SpawnReq{Title: "next"})
	must(t, err)
	if got := agent.Recorded(filepath.Join(a.Root, ".saddle", "run", next.ID)); got != usage.Grok {
		t.Fatalf("next spawn ran %s, want grok", got)
	}
	ft.screens[w.Window] = "⏺ Picking up where I left off"
	must(t, e.Tick())
	if len(a.ExhaustedAdapters(time.Now())) != 0 {
		t.Fatal("claude still out after its banner cleared")
	}
}

func allAdapters() []agent.Status {
	var out []agent.Status
	for _, n := range agent.Names() {
		out = append(out, agent.Status{Name: n, OK: true})
	}
	return out
}

// #481: a claude worker that runs out mid-task is handed to grok instead
// of parking: a new agent in a new window and the same worktree, running,
// with no "parked" notice, and claude stays out for new spawns.
func TestEngineHandsOffWorkerOnUsageLimit(t *testing.T) {
	a, ft := setup(t)
	a.AdapterStatus = allAdapters
	a.OwnerNotify = func(string) {}
	w, err := a.Spawn(app.SpawnReq{Title: "meter", Claims: []string{"meter/**"}})
	must(t, err)
	clock := time.Now()
	e := New(a)
	e.now = func() time.Time { return clock }
	ft.screens[w.Window] = "You've hit your session limit · resets in 3 hours\n> "
	must(t, e.Tick())
	clock = clock.Add(5 * time.Second)
	must(t, e.Tick())
	got, err := a.Store.Task(w.ID)
	must(t, err)
	if got.Status != store.Running || got.Window == w.Window || got.Worktree != w.Worktree {
		t.Fatalf("after the limit: %+v", got)
	}
	if ad := a.TaskAdapterName(w.ID); ad != usage.Grok {
		t.Fatalf("task runs on %s, want grok", ad)
	}
	if n := orchNotices(t, a); strings.Contains(n, "parked") {
		t.Fatalf("a handed-off worker was reported parked: %q", n)
	}
	ft.screens[got.Window] = "working on it"
	clock = clock.Add(10 * time.Second)
	must(t, e.Tick())
	if st, _ := a.Store.Task(w.ID); st.Status != store.Running {
		t.Fatalf("finisher status %q", st.Status)
	}
	if _, out := a.ExhaustedAdapters(time.Now())[usage.Claude]; !out {
		t.Fatal("claude no longer out of quota")
	}
}

// codexLimitScreen is codex after its usage-limit error: the turn ended and
// it waits at its composer. It won't continue on its own.
const codexLimitScreen = `• Ran make check
■ You've hit your usage limit. Upgrade to Pro (https://openai.com/chatgpt/pricing) or try again in 2 hours 5 minutes.

› Ask Codex to do anything`

// #480: a codex (or grok) worker parked on its usage limit used to stay
// paused forever: the notice promised it would continue on its own, as
// Claude Code does. Once its adapter's quota is back, saddle types it on.
func TestEngineResumesCodexAfterItsReset(t *testing.T) {
	a, ft := setup(t)
	a.Cfg.AdapterPolicy.Failover = config.FailoverPark
	a.AdapterStatus = allAdapters
	w, err := a.Spawn(app.SpawnReq{Title: "meter", Adapter: usage.Codex})
	must(t, err)
	clock := time.Now()
	e := New(a)
	e.now = func() time.Time { return clock }
	ft.screens[w.Window] = "• Working (3s • esc to interrupt)"
	must(t, e.Tick())
	orchNotices(t, a)

	ft.screens[w.Window] = codexLimitScreen
	must(t, e.Tick())
	clock = clock.Add(5 * time.Second)
	must(t, e.Tick())
	if st, _ := a.Store.Task(w.ID); st.Status != app.StatusPaused {
		t.Fatalf("status %q while the banner is up", st.Status)
	}
	got := orchNotices(t, a)
	if strings.Contains(got, "continues on its own") || !strings.Contains(got, "codex") || !strings.Contains(got, "won't continue on its own") {
		t.Fatalf("notices %q", got)
	}
	clock = clock.Add(time.Hour)
	must(t, e.Tick())
	if len(ft.texts[w.Window]) != 0 {
		t.Fatalf("typed before the reset: %q", ft.texts[w.Window])
	}

	clock = clock.Add(2 * time.Hour)
	must(t, e.Tick())
	if txt := ft.texts[w.Window]; len(txt) != 1 || txt[0] != app.LimitResumeLine {
		t.Fatalf("typed %q after the reset", txt)
	}
	if st, _ := a.Store.Task(w.ID); st.Status != store.Running {
		t.Fatalf("status %q after resuming", st.Status)
	}
	// The old banner still sits in its scrollback: not a new limit.
	for range 3 {
		clock = clock.Add(5 * time.Second)
		must(t, e.Tick())
	}
	if st, _ := a.Store.Task(w.ID); st.Status == app.StatusPaused {
		t.Fatal("re-parked on the old banner")
	}
	if _, out := a.ExhaustedAdapters(clock)[usage.Codex]; out {
		t.Fatal("the old banner marked codex out of quota again")
	}
	if len(ft.texts[w.Window]) != 1 {
		t.Fatalf("typed again: %q", ft.texts[w.Window])
	}
}

// #480: only the Stop hook marked a worker idle, so a codex worker (no
// hooks) that stopped without calling done stayed "running" and nobody was
// told. Its screen sitting still with no working indicator marks it idle;
// working again marks it running. Hooked workers are left to their hooks.
func TestEngineMarksHooklessWorkerIdleFromItsScreen(t *testing.T) {
	a, ft := setup(t)
	a.AdapterStatus = allAdapters
	cx, err := a.Spawn(app.SpawnReq{Title: "codex work", Adapter: usage.Codex})
	must(t, err)
	cl, err := a.Spawn(app.SpawnReq{Title: "claude work"})
	must(t, err)
	clock := time.Now()
	e := New(a)
	e.now = func() time.Time { return clock }
	ft.screens[cx.Window] = "• Working (3s • esc to interrupt)"
	ft.screens[cl.Window] = "⏺ Done.\n> "
	must(t, e.Tick())
	orchNotices(t, a)

	clock = clock.Add(time.Minute) // still working: the timer froze, say
	must(t, e.Tick())
	if st, _ := a.Store.Task(cx.ID); st.Status != store.Running {
		t.Fatalf("working codex marked %q", st.Status)
	}
	ft.screens[cx.Window] = "• I finished the change.\n\n› Ask Codex to do anything"
	must(t, e.Tick())
	clock = clock.Add(5 * time.Second)
	must(t, e.Tick())
	if st, _ := a.Store.Task(cx.ID); st.Status != store.Running {
		t.Fatalf("codex marked %q after only 5s", st.Status)
	}
	clock = clock.Add(app.HooklessIdleAfter)
	must(t, e.Tick())
	if st, _ := a.Store.Task(cx.ID); st.Status != store.Idle {
		t.Fatalf("codex sitting at its composer is %q, want idle", st.Status)
	}
	if got := orchNotices(t, a); !strings.Contains(got, cx.ID+" (codex work) stopped without calling done") {
		t.Fatalf("notices %q", got)
	}
	if st, _ := a.Store.Task(cl.ID); st.Status != store.Running {
		t.Fatalf("hooked claude worker marked %q from its screen", st.Status)
	}

	ft.screens[cx.Window] = "• Working (1s • esc to interrupt)"
	must(t, e.Tick())
	if st, _ := a.Store.Task(cx.ID); st.Status != store.Running {
		t.Fatalf("codex working again is %q", st.Status)
	}
}

// batchTmux is a screenTmux that batches like tmux.Tmux and counts the tmux
// processes each call would start.
type batchTmux struct {
	*screenTmux
	execs int
}

func (b *batchTmux) Alive(w string) bool { b.execs++; return b.screenTmux.Alive(w) }
func (b *batchTmux) Capture(w string, n int) (string, error) {
	b.execs++
	return b.screenTmux.Capture(w, n)
}
func (b *batchTmux) Windows() (map[string]bool, error) {
	b.execs++
	ws := map[string]bool{}
	for w := range b.screens {
		ws[w] = true
	}
	return ws, nil
}
func (b *batchTmux) CaptureMany(lines map[string]int) (map[string]string, error) {
	b.execs++
	out := map[string]string{}
	for w := range lines {
		out[w] = b.screens[w]
	}
	return out, nil
}

// With 16 idle workers a tick starts two tmux processes, not two per
// worker (#272).
func TestEngineTickTmuxExecs(t *testing.T) {
	a, ft := setup(t)
	bt := &batchTmux{screenTmux: ft}
	a.Tmux = bt
	for i := 1; i <= 16; i++ {
		w := fmt.Sprintf("@%d", i)
		ft.screens[w] = "idle " + w
		must(t, a.Store.CreateTask(store.Task{ID: fmt.Sprintf("t%d", i), Title: "idle", Status: store.Idle, Window: w}))
	}
	e := New(a)
	must(t, e.Tick())
	bt.execs = 0
	must(t, e.Tick())
	if bt.execs > 2 {
		t.Fatalf("a steady tick ran %d tmux processes with 16 idle workers, want at most 2", bt.execs)
	}
	if got := e.screens["t7"]; got == nil || got.text != "idle @7" {
		t.Fatalf("t7's screen = %+v, want it read", got)
	}
}
