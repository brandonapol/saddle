package app

import (
	"strings"
	"testing"
	"time"

	"github.com/brandonapol/saddle/internal/autopilot"
	"github.com/brandonapol/saddle/internal/config"
	"github.com/brandonapol/saddle/internal/usage"
)

// disableGrok turns grok off as the owner did in #839.
func disableGrok(a *App) {
	if a.Cfg.Adapters == nil {
		a.Cfg.Adapters = config.AdapterMap{}
	}
	ad := a.Cfg.Adapters[usage.Grok]
	ad.Disabled = true
	a.Cfg.Adapters[usage.Grok] = ad
}

// #839: the chooser never ranks a disabled adapter, not even when it is the
// session default, and status shows it as disabled with no pick rank.
func TestChooserSkipsDisabledAdapter(t *testing.T) {
	a, _ := setup(t)
	a.Cfg.AdapterOrder = []string{"grok", "claude", "codex"}
	disableGrok(a)
	now := time.Now()
	for _, pol := range config.Picks() {
		a.Cfg.AdapterPolicy.Pick = pol
		for _, def := range []string{"claude", "grok"} {
			rs := a.rankAdapters(pickReq{def: def, claims: true}, nil, now)
			if got := picked(rs); strings.Contains(got, "grok") || got == "" {
				t.Errorf("policy %s, default %s: picked %q", pol, def, got)
			}
		}
	}
	var seen bool
	for _, s := range a.Adapters() {
		if s.Name != usage.Grok {
			continue
		}
		seen = true
		if !s.Disabled || s.OK || s.PickRank != 0 || !strings.Contains(s.Pick, "disabled") {
			t.Errorf("grok status = %+v, want disabled, not OK, rank 0", s)
		}
	}
	if !seen {
		t.Fatal("status lists no grok")
	}
}

// #839: with grok disabled and claude out of quota, a ci-red repair spawn
// goes to codex; with codex out too it is refused (parks). It never starts
// grok, the session default or not.
func TestRepairSpawnSkipsDisabledAdapter(t *testing.T) {
	for _, harness := range []string{config.HarnessClaude, config.HarnessGrok} {
		t.Run(harness, func(t *testing.T) {
			a, _, _ := rotationApp(t)
			a.Cfg.Harness = harness
			a.Cfg.AdapterOrder = []string{"claude", "grok", "codex"}
			disableGrok(a)
			must(t, a.MarkExhausted("claude", time.Now().Add(time.Hour), ""))
			fix, err := a.Spawn(SpawnReq{Title: "fix CI on t1", Prompt: "fix it", Parent: OrchestratorID, ModelHint: "sonnet", Confirm: true})
			must(t, err)
			if fix.Adapter != usage.Codex {
				t.Fatalf("repair went to %q, want codex", fix.Adapter)
			}
			must(t, a.MarkExhausted("codex", time.Now().Add(time.Hour), ""))
			if tk, err := a.Spawn(SpawnReq{Title: "fix CI on t2", Parent: OrchestratorID, Confirm: true}); err == nil {
				t.Fatalf("spawn with only grok left ran %q, want refused", tk.Adapter)
			}
		})
	}
}

// #839: adapter=grok on a disabled grok is refused with a message saying
// how to enable it, even with force.
func TestExplicitDisabledAdapterRefused(t *testing.T) {
	a, _ := setup(t)
	disableGrok(a)
	for _, force := range []bool{false, true} {
		_, err := a.Spawn(SpawnReq{Title: "on grok", Adapter: usage.Grok, Force: force})
		if err == nil || !strings.Contains(err.Error(), "disabled") || !strings.Contains(err.Error(), "[adapters.grok]") {
			t.Errorf("force=%v: err = %v, want a disabled refusal", force, err)
		}
	}
	if _, _, _, err := a.RoutePreview(SpawnReq{Adapter: usage.Grok}); err == nil {
		t.Error("route preview allows a disabled adapter")
	}
}

// #839: a worker whose adapter runs out is never handed to a disabled one:
// grok would win (hooked, for a task with claims), so codex takes it.
func TestHandoffSkipsDisabledAdapter(t *testing.T) {
	a, _, _ := rotationApp(t)
	a.Cfg.AdapterOrder = []string{"claude", "codex", "grok"}
	a.Cfg.AdapterPolicy.Pick = config.PickOrder
	disableGrok(a)
	tk, err := a.Spawn(SpawnReq{Title: "meter", Prompt: "build the meter", Claims: []string{"meter/**"}})
	must(t, err)
	to, err := a.LimitHit(tk.ID, usage.LimitBanner{Resets: "in 3 hours"})
	must(t, err)
	if to != usage.Codex {
		t.Fatalf("handed to %q, want codex", to)
	}
}

// #839: a [fallback] chain passes over a disabled entry.
func TestFallbackChainSkipsDisabledAdapter(t *testing.T) {
	a, _ := setup(t)
	a.Cfg.Fallback = map[string][]string{"claude": {"grok", "codex"}}
	disableGrok(a)
	p, skipped, ok := a.chainPick("claude", "", map[string]Exhaustion{"claude": {Adapter: "claude", Until: time.Now().Add(time.Hour)}}, nil)
	if !ok || p.Adapter != usage.Codex {
		t.Fatalf("chain picked %+v (ok %v), want codex", p, ok)
	}
	if len(skipped) != 1 || !strings.Contains(skipped[0], "disabled") {
		t.Errorf("skipped = %q, want grok as disabled", skipped)
	}
}

// #839: autopilot drops a ticket's "Provider: grok" line when grok is
// disabled and lets the chooser pick, instead of failing the ticket.
func TestAutopilotSkipsDisabledProvider(t *testing.T) {
	a, _ := setup(t)
	a.Cfg.AdapterOrder = []string{"claude", "grok", "codex"}
	disableGrok(a)
	env := &autopilotEnv{a: a}
	ad, _, _, err := env.Route(autopilot.SpawnReq{Title: "ticket", Adapter: usage.Grok})
	must(t, err)
	if ad == usage.Grok || ad == "" {
		t.Fatalf("autopilot routes to %q", ad)
	}
}
