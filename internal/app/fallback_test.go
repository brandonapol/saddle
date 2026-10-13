package app

import (
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/agent"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/usage"
)

// fallbackApp runs codex on sol by default, with sol's chain skipping an
// excluded model before claude:opus and grok.
func fallbackApp(t *testing.T) (*App, *fakeTmux, *ownerSink) {
	t.Helper()
	a, ft, sink := rotationApp(t)
	a.Cfg.Harness = config.HarnessCodex
	a.Cfg.Adapters = config.AdapterMap{usage.Codex: {Model: "gpt-6-sol"}}
	a.Cfg.Fallback = map[string][]string{"codex:sol": {"codex:astra", "claude:opus", "grok"}}
	return a, ft, sink
}

// #483: a sol worker hitting its limit marks only codex:sol out. The worker
// is handed to claude:opus (astra is excluded, so skipped), the owner hears
// once who ran out, who took over and the reset, new spawns that would run
// sol go to claude:opus, and codex's other models stay usable.
func TestModelLimitFallsBackDownItsChain(t *testing.T) {
	a, ft, sink := fallbackApp(t)
	w, err := a.Spawn(SpawnReq{Title: "sol work", Prompt: "build it"})
	must(t, err)
	if w.Model != "gpt-6-sol" || agent.Recorded(a.stateDir("run", w.ID)) != usage.Codex {
		t.Fatalf("spawned on %s model %q", agent.Recorded(a.stateDir("run", w.ID)), w.Model)
	}
	must(t, a.Store.SetStatus(w.ID, StatusPaused))
	old := w.Window
	_ = orchNotices(t, a, "")

	to, err := a.LimitHit(w.ID, usage.LimitBanner{Resets: "in 3 hours"})
	must(t, err)
	if to != usage.Claude {
		t.Fatalf("handed to %q, want claude", to)
	}
	got, _ := a.Store.Task(w.ID)
	if got.Model != "opus" || agent.Recorded(a.stateDir("run", w.ID)) != usage.Claude || ft.Alive(old) {
		t.Fatalf("after the handoff: model %q on %s, old window alive %v", got.Model, agent.Recorded(a.stateDir("run", w.ID)), ft.Alive(old))
	}
	if !runDirHas(t, a, w.ID, "ran out of quota on codex:sol") {
		t.Error("the finisher isn't told sol ran out")
	}
	if ex := a.ExhaustedAdapters(time.Now()); len(ex) != 0 {
		t.Errorf("adapters out: %v, want none", ex)
	}
	if _, out := a.ExhaustedModels(time.Now())["codex:sol"]; !out {
		t.Fatal("codex:sol not out")
	}
	// The same limit again says nothing new.
	must(t, a.MarkModelExhausted(usage.Codex, "sol", time.Now().Add(time.Hour), ""))
	told := sink.all()
	if len(told) != 1 || !strings.Contains(told[0], "codex:sol is out of quota until") || !strings.Contains(told[0], "go to claude:opus") ||
		!strings.Contains(told[0], "other codex models stay usable") {
		t.Fatalf("owner told %q", told)
	}

	next, err := a.Spawn(SpawnReq{Title: "would be sol"})
	must(t, err)
	if agent.Recorded(a.stateDir("run", next.ID)) != usage.Claude || next.Model != "opus" {
		t.Fatalf("next spawn on %s model %q, want claude opus", agent.Recorded(a.stateDir("run", next.ID)), next.Model)
	}
	if ev, _ := a.lastEvent(next.ID, EventAdapterRotated); ev != "codex:sol -> claude" {
		t.Errorf("rotated event %q", ev)
	}
	terra, err := a.Spawn(SpawnReq{Title: "terra work", Model: "gpt-6-terra"})
	must(t, err)
	if agent.Recorded(a.stateDir("run", terra.ID)) != usage.Codex || terra.Model != "gpt-6-terra" {
		t.Fatalf("terra spawn on %s model %q, want codex terra", agent.Recorded(a.stateDir("run", terra.ID)), terra.Model)
	}
	if _, err := a.Spawn(SpawnReq{Title: "explicit sol", Adapter: usage.Codex}); err == nil || !strings.Contains(err.Error(), "model codex:sol is out of quota") ||
		!strings.Contains(err.Error(), "claude:opus") {
		t.Errorf("explicit codex on sol: %v", err)
	}

	for _, s := range a.Adapters() {
		if s.Name == usage.Codex && (s.OutOfQuotaUntil != "" || s.ModelsOutOfQuota["sol"] == "") {
			t.Errorf("codex status %+v", s)
		}
	}
	fs := a.Fallbacks(time.Now())
	if len(fs) != 1 || fs[0].From != "codex:sol" || fs[0].OutOfQuotaUntil == "" || fs[0].Now != "claude:opus" ||
		len(fs[0].Skipped) != 1 || !strings.Contains(fs[0].Skipped[0], "codex:astra is excluded") {
		t.Errorf("fallbacks %+v", fs)
	}
	if b := a.RotationBanner(time.Now()); !strings.Contains(b, "codex:sol until") || !strings.Contains(b, "new spawns go to claude:opus") {
		t.Errorf("banner %q", b)
	}
}

// #483: chain entries that are out of quota are skipped too, and once sol's
// reset passes spawns run it again and the orchestrator hears it is back.
func TestModelFallbackSkipsOutEntriesAndResets(t *testing.T) {
	t.Parallel()
	a, _, _ := fallbackApp(t)
	must(t, a.MarkExhausted(usage.Claude, time.Now().Add(time.Hour), ""))
	must(t, a.MarkModelExhausted(usage.Codex, "sol", time.Now().Add(80*time.Millisecond), ""))
	g, err := a.Spawn(SpawnReq{Title: "would be sol"})
	must(t, err)
	if r := agent.Recorded(a.stateDir("run", g.ID)); r != usage.Grok {
		t.Fatalf("spawn ran %s, want grok (claude is out)", r)
	}
	time.Sleep(100 * time.Millisecond)
	s, err := a.Spawn(SpawnReq{Title: "sol again"})
	must(t, err)
	if r := agent.Recorded(a.stateDir("run", s.ID)); r != usage.Codex || s.Model != "gpt-6-sol" {
		t.Fatalf("after the reset spawn ran %s model %q, want codex sol", r, s.Model)
	}
	if ev, ok := a.lastEvent(OrchestratorID, EventAdapterRestored); !ok || ev != "codex:sol" {
		t.Errorf("restored event %q %v", ev, ok)
	}
}

// #483: without a [fallback] key a model's limit still marks the whole
// adapter out, as before.
func TestLimitWithoutChainMarksAdapter(t *testing.T) {
	t.Parallel()
	a, _, _ := fallbackApp(t)
	a.Cfg.Fallback = nil
	a.Cfg.AdapterPolicy.Failover = config.FailoverPark
	w, err := a.Spawn(SpawnReq{Title: "sol work"})
	must(t, err)
	must(t, a.AdapterLimitHit(w.ID, usage.LimitBanner{Resets: "in 3 hours"}))
	if _, out := a.ExhaustedAdapters(time.Now())[usage.Codex]; !out {
		t.Fatal("codex not out")
	}
	if len(a.ExhaustedModels(time.Now())) != 0 {
		t.Fatal("a model marked out without a chain")
	}
}

// #483: an adapter key's chain goes ahead of [adapters] policy while that
// adapter is out.
func TestAdapterChainOverridesPolicy(t *testing.T) {
	t.Parallel()
	a, _, _ := fallbackApp(t)
	a.Cfg.Harness = config.HarnessClaude
	a.Cfg.Fallback = map[string][]string{"claude": {"codex:terra"}}
	must(t, a.MarkExhausted(usage.Claude, time.Now().Add(time.Hour), ""))
	c, err := a.Spawn(SpawnReq{Title: "claude out", Model: "opus"})
	must(t, err)
	if r := agent.Recorded(a.stateDir("run", c.ID)); r != usage.Codex || c.Model != "terra" {
		t.Fatalf("spawn ran %s model %q, want codex terra (order alone picks grok)", r, c.Model)
	}
}
