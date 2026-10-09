package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/store"
	"github.com/brandonapol/saddle/internal/usage"
)

// ownerSink records owner notifications instead of popping them.
type ownerSink struct {
	mu   sync.Mutex
	sent []string
}

func (s *ownerSink) notify(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, text)
}

func (s *ownerSink) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

func rotationApp(t *testing.T) (*App, *fakeTmux, *ownerSink) {
	t.Helper()
	a, ft := setup(t)
	sink := &ownerSink{}
	a.OwnerNotify = sink.notify
	return a, ft, sink
}

// #321: under saddle up grok every spawn that names no adapter runs grok,
// whichever path made it, and never inherits a Claude model.
func TestHarnessGrokSpawnsGrokOnEveryDefaultPath(t *testing.T) {
	t.Parallel()
	a, ft := setup(t)
	a.Cfg.Harness = config.HarnessGrok
	a.Cfg.Grok = config.Grok{Cmd: "grok", Model: "grok-4.5", PermissionMode: "bypassPermissions"}

	var ids []string
	plain, err := a.Spawn(SpawnReq{Title: "plain spawn"})
	must(t, err)
	ids = append(ids, plain.ID)
	// The CI-red repair asks for sonnet; the CI fix and the stuck-task repair
	// name nothing.
	cired, err := a.Spawn(SpawnReq{Title: "ci repair", Model: "sonnet", Parent: OrchestratorID, Confirm: true})
	must(t, err)
	ids = append(ids, cired.ID)
	env := &autopilotEnv{a: a}
	ap, err := env.Spawn(autopilot.SpawnReq{Title: "autopilot top-up", Model: "opus"})
	must(t, err)
	ids = append(ids, ap)

	// A task whose run dir never recorded an adapter resumes on the harness.
	lost, err := a.Spawn(SpawnReq{Title: "lost window"})
	must(t, err)
	must(t, os.Remove(filepath.Join(a.stateDir("run", lost.ID), "adapter")))
	must(t, ft.KillWindow(lost.Window))
	_, err = a.Resume(lost.ID)
	must(t, err)
	ids = append(ids, lost.ID)

	for _, id := range ids {
		task, err := a.Store.Task(id)
		must(t, err)
		if got := agent.Recorded(a.stateDir("run", id)); got != usage.Grok {
			t.Errorf("%s (%s) launched %s, want grok", id, task.Title, got)
		}
		if task.Model != "grok-4.5" {
			t.Errorf("%s (%s) model %q, want grok's", id, task.Title, task.Model)
		}
		if s := launchScript(t, a, id); strings.Contains(s, "'claude'") || strings.Contains(s, "'sonnet'") || strings.Contains(s, "'opus'") {
			t.Errorf("%s (%s) launch script runs claude:\n%s", id, task.Title, s)
		}
	}
}

// #321: claude running out routes the next default spawn to grok and tells
// the owner once: TUI/orchestrator notice plus the configured notifier.
func TestClaudeExhaustionRoutesNextSpawnToGrok(t *testing.T) {
	t.Parallel()
	a, _, sink := rotationApp(t)
	w, err := a.Spawn(SpawnReq{Title: "claude work"})
	must(t, err)
	_ = orchNotices(t, a, "")
	must(t, a.AdapterLimitHit(w.ID, usage.LimitBanner{Resets: "3pm"}))
	must(t, a.AdapterLimitHit(w.ID, usage.LimitBanner{Resets: "3pm"})) // the same banner again

	ns := orchNotices(t, a, "out of quota")
	if len(ns) != 1 || !strings.Contains(ns[0], "claude") || !strings.Contains(ns[0], "grok") || !strings.Contains(ns[0], "3pm") {
		t.Fatalf("orchestrator notices = %q, want one naming claude, grok and the reset", ns)
	}
	if got := sink.all(); len(got) != 1 || got[0] != ns[0] {
		t.Fatalf("owner notifications = %q", got)
	}
	if b := a.RotationBanner(time.Now()); !strings.Contains(b, "claude") || !strings.Contains(b, "grok") {
		t.Errorf("banner = %q", b)
	}
	if task, _ := a.Store.Task(w.ID); task.Status == store.Killed {
		t.Error("the parked task was killed")
	}

	next, err := a.Spawn(SpawnReq{Title: "after claude ran out", Model: "opus"})
	must(t, err)
	if got := agent.Recorded(a.stateDir("run", next.ID)); got != usage.Grok {
		t.Fatalf("next spawn ran %s, want grok", got)
	}
	if next.Model == "opus" {
		t.Errorf("rotated spawn kept the claude model %q", next.Model)
	}
	if _, err := a.Spawn(SpawnReq{Title: "explicit claude", Adapter: usage.Claude}); err == nil || !strings.Contains(err.Error(), "out of quota") {
		t.Errorf("explicit claude while exhausted: err = %v", err)
	}
}

// #321: rotating a claimed task onto codex, which has no hooks, tells the
// owner its claims are advisory.
func TestRotationOntoHooklessAdapterWarnsAboutClaims(t *testing.T) {
	t.Parallel()
	a, _, _ := rotationApp(t)
	far := time.Now().Add(time.Hour)
	must(t, a.MarkExhausted(usage.Claude, far, ""))
	must(t, a.MarkExhausted(usage.Grok, far, ""))
	_ = orchNotices(t, a, "")
	c, err := a.Spawn(SpawnReq{Title: "claimed", Claims: []string{"billing/**"}})
	must(t, err)
	if got := agent.Recorded(a.stateDir("run", c.ID)); got != usage.Codex {
		t.Fatalf("spawn ran %s, want codex", got)
	}
	if ns := orchNotices(t, a, "advisory"); len(ns) != 1 || !strings.Contains(ns[0], c.ID) {
		t.Errorf("claims warning = %q", ns)
	}
}

// #321: with every adapter out, the owner gets a distinct notice and new
// spawns park as paused launches until the first reset.
func TestAllAdaptersExhausted(t *testing.T) {
	t.Parallel()
	a, _, sink := rotationApp(t)
	soon, later := time.Now().Add(30*time.Minute), time.Now().Add(2*time.Hour)
	must(t, a.MarkExhausted(usage.Claude, later, "5pm"))
	must(t, a.MarkExhausted(usage.Grok, soon, "3pm"))
	_ = orchNotices(t, a, "")
	must(t, a.MarkExhausted(usage.Codex, later, ""))
	ns := orchNotices(t, a, "All adapters")
	if len(ns) != 1 || !strings.Contains(ns[0], "grok") || !strings.Contains(ns[0], "3pm") {
		t.Fatalf("all-exhausted notices = %q, want one naming the first reset (grok, 3pm)", ns)
	}
	if got := sink.all(); len(got) != 3 || !strings.Contains(got[2], "All adapters") {
		t.Fatalf("owner notifications = %q", got)
	}
	_, err := a.Spawn(SpawnReq{Title: "nowhere to run"})
	if !errors.Is(err, ErrLaunchesPaused) {
		t.Fatalf("spawn with every adapter out: err = %v, want ErrLaunchesPaused", err)
	}
	if ts, _ := a.Store.Tasks(); len(ts) != 0 {
		t.Fatalf("refused spawn left tasks: %+v", ts)
	}
}

// #321: once its reset passes, an adapter takes default spawns again; the
// orchestrator's digest says it is back, without pinging the owner.
func TestAdapterResetReenables(t *testing.T) {
	t.Parallel()
	a, _, sink := rotationApp(t)
	must(t, a.MarkExhausted(usage.Claude, time.Now().Add(50*time.Millisecond), ""))
	time.Sleep(60 * time.Millisecond)
	_ = orchNotices(t, a, "")
	c, err := a.Spawn(SpawnReq{Title: "after the reset"})
	must(t, err)
	if got := agent.Recorded(a.stateDir("run", c.ID)); got != usage.Claude {
		t.Fatalf("spawn after the reset ran %s, want claude", got)
	}
	if len(a.ExhaustedAdapters(time.Now())) != 0 {
		t.Error("claude still listed as exhausted")
	}
	if got, ok := a.lastEvent(OrchestratorID, EventAdapterRestored); !ok || got != usage.Claude {
		t.Errorf("restored event = %q %v", got, ok)
	}
	if got := sink.all(); len(got) != 1 {
		t.Errorf("owner notifications = %q", got)
	}
	if a.RotationBanner(time.Now()) != "" {
		t.Error("banner still shown")
	}
}

// #321: a lost task whose adapter is out of quota resumes on the next one.
func TestResumeMovesLostTaskOffExhaustedAdapter(t *testing.T) {
	t.Parallel()
	a, ft, _ := rotationApp(t)
	w, err := a.Spawn(SpawnReq{Title: "claude work"})
	must(t, err)
	must(t, a.MarkExhausted(usage.Claude, time.Now().Add(time.Hour), ""))
	must(t, ft.KillWindow(w.Window))
	_, err = a.Resume(w.ID)
	must(t, err)
	if got := agent.Recorded(a.stateDir("run", w.ID)); got != usage.Grok {
		t.Fatalf("resumed on %s, want grok", got)
	}
	if task, _ := a.Store.Task(w.ID); task.Model == a.Cfg.Claude.Model {
		t.Errorf("resumed on grok with claude model %q", task.Model)
	}
}

func TestParseReset(t *testing.T) {
	t.Parallel()
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	now := time.Date(2026, 10, 9, 13, 0, 0, 0, ny)
	for _, c := range []struct {
		in   string
		want time.Time
	}{
		{"3pm (America/New_York)", time.Date(2026, 10, 9, 15, 0, 0, 0, ny)},
		{"12:10pm (America/New_York)", time.Date(2026, 10, 10, 12, 10, 0, 0, ny)},
		{"9am (America/New_York)", time.Date(2026, 10, 10, 9, 0, 0, 0, ny)},
		{"4:05 PM", time.Date(2026, 10, 9, 16, 5, 0, 0, ny)},
		{"Oct 12, 3pm (America/New_York)", time.Date(2026, 10, 12, 15, 0, 0, 0, ny)},
	} {
		got, ok := parseReset(c.in, now)
		if !ok || !got.Equal(c.want) {
			t.Errorf("parseReset(%q) = %v %v, want %v", c.in, got, ok, c.want)
		}
	}
	if _, ok := parseReset("soon", now); ok {
		t.Error("parsed a reset from nothing")
	}
}

// #321: the orchestrator brief lists the three providers with their models
// and strengths, tells it to pick per task, and explains rotation.
func TestOrchestratorBriefListsProviders(t *testing.T) {
	t.Parallel()
	a, _ := setup(t)
	b := a.orchestratorBrief()
	for _, want := range []string{"Anthropic", "SpaceXAI", "OpenAI", "claude", "grok", "codex", "pick per task", "out of quota", "[adapters] order"} {
		if !strings.Contains(b, want) {
			t.Errorf("brief lacks %q", want)
		}
	}
	if !strings.Contains(a.PluginBrief(), "SpaceXAI") {
		t.Error("plugin brief lacks the providers")
	}
}

// #321: status lists each adapter's provider, hooks and quota state.
func TestAdaptersStatusShowsProviderAndQuota(t *testing.T) {
	t.Parallel()
	a, _, _ := rotationApp(t)
	must(t, a.MarkExhausted(usage.Claude, time.Now().Add(time.Hour), "3pm"))
	got := map[string]agent.Status{}
	for _, s := range a.Adapters() {
		got[s.Name] = s
	}
	if c := got[usage.Claude]; c.Provider != "Anthropic" || !c.Hooks || c.OutOfQuotaUntil != "3pm" {
		t.Errorf("claude = %+v", c)
	}
	if g := got[usage.Grok]; g.Provider != "SpaceXAI" || !g.Hooks || g.OutOfQuotaUntil != "" {
		t.Errorf("grok = %+v", g)
	}
	if c := got[usage.Codex]; c.Provider != "OpenAI" || c.Hooks {
		t.Errorf("codex = %+v", c)
	}
}

// #321: a claimed task resumed onto codex (no hooks) tells the owner its
// claims are now advisory.
func TestResumeOntoHooklessAdapterWarnsAboutClaims(t *testing.T) {
	t.Parallel()
	a, ft, _ := rotationApp(t)
	w, err := a.Spawn(SpawnReq{Title: "claimed work", Claims: []string{"billing/**"}})
	must(t, err)
	far := time.Now().Add(time.Hour)
	must(t, a.MarkExhausted(usage.Claude, far, ""))
	must(t, a.MarkExhausted(usage.Grok, far, ""))
	_ = orchNotices(t, a, "")
	must(t, ft.KillWindow(w.Window))
	_, err = a.Resume(w.ID)
	must(t, err)
	if got := agent.Recorded(a.stateDir("run", w.ID)); got != usage.Codex {
		t.Fatalf("resumed on %s, want codex", got)
	}
	if ns := orchNotices(t, a, "advisory"); len(ns) != 1 || !strings.Contains(ns[0], w.ID) {
		t.Errorf("claims warning = %q", ns)
	}
}
